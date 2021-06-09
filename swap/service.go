package swap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/sputn1ck/liquid-loop/lightning"
	"github.com/sputn1ck/liquid-loop/liquid"
	"github.com/sputn1ck/liquid-loop/wallet"
	"github.com/vulpemventures/go-elements/elementsutil"
	"github.com/vulpemventures/go-elements/network"
	"github.com/vulpemventures/go-elements/payment"
	"github.com/vulpemventures/go-elements/pset"
	"github.com/vulpemventures/go-elements/transaction"
	"sync"
)

const (
	FIXED_FEE = 500
	LOCKTIME  = 100
)


type TxBuilder interface {
}

type Wallet interface {
	GetBalance() (uint64, error)
	GetPubkey() (*btcec.PublicKey, error)
	GetPrivKey() (*btcec.PrivateKey, error)
	GetUtxos(amount uint64) ([]*wallet.Utxo, uint64, error)
}

type SwapStore interface {
	Create(context.Context, *Swap) error
	Update(context.Context, *Swap) error
	DeleteById(context.Context, string) error
	GetById(context.Context, string) (*Swap, error)
	ListAll(context.Context) ([]*Swap, error)
}

type Service struct {
	store      SwapStore
	wallet     Wallet
	pc         lightning.PeerCommunicator
	blockchain wallet.BlockchainService
	lightning  LightningClient
	network    *network.Network
	asset      []byte

	txWatchList map[string]string
	sync.Mutex
	ctx context.Context
}

func NewService(store SwapStore, wallet Wallet, pc lightning.PeerCommunicator, blockchain wallet.BlockchainService, lightning LightningClient, network *network.Network) *Service {
	watchList := make(map[string]string)
	ctx := context.Background()
	return &Service{
		store: store,
		wallet: wallet,
		pc: pc,
		blockchain: blockchain,
		lightning: lightning,
		network: network,
		txWatchList: watchList,
		ctx: ctx}
}

func (s *Service) ListSwaps() ([]*Swap, error) {
	return s.store.ListAll(context.Background())
}

func (s *Service) StartSwapIn( peerNodeId string, channelId string, amount uint64) error {
	swap := NewSwap(SWAPTYPE_IN, amount, peerNodeId, channelId)
	err := s.store.Create(s.ctx, swap)
	if err != nil {
		return err
	}
	pubkey, err := s.wallet.GetPubkey()
	if err != nil {
		return err
	}
	request := &SwapRequest{
		SwapId:          swap.Id,
		ChannelId:       channelId,
		Amount:          amount,
		Type:            SWAPTYPE_OUT,
		TakerPubkeyHash: hex.EncodeToString(pubkey.SerializeCompressed()),
	}
	err = s.pc.SendMessage(peerNodeId, request)
	if err != nil {
		return err
	}
	swap.State = SWAPSTATE_REQUEST_SENT
	err = s.store.Update(s.ctx, swap)
	if err != nil {
		return err
	}
	return nil
}

// todo implement loop in
func (s *Service) OnSwapRequest(senderNodeId string, request SwapRequest) error {
	ctx := context.Background()
	swap := &Swap{
		Id:         request.SwapId,
		Type:       request.Type,
		State:      SWAPSTATE_REQUEST_RECEIVED,
		PeerNodeId: senderNodeId,
		Amount:     request.Amount,
		ChannelId:  request.ChannelId,
	}

	err := s.store.Create(s.ctx, swap)
	if err != nil {
		return err
	}

	pubkey, err := s.wallet.GetPubkey()
	if err != nil {
		return err
	}

	// requester wants to swap out, meaning responder is the maker
	if request.Type == SWAPTYPE_OUT {
		swap.TakerPubkeyHash = request.TakerPubkeyHash
		swap.MakerPubkeyHash = hex.EncodeToString(pubkey.SerializeCompressed())

		// Generate Preimage
		var preimage lightning.Preimage

		if _, err = rand.Read(preimage[:]); err != nil {
			return err
		}
		pHash := preimage.Hash()

		payreq, err := s.lightning.GetPayreq((request.Amount+FIXED_FEE)*1000, preimage.String(), swap.Id)
		if err != nil {
			return err
		}

		swap.Payreq = payreq
		swap.PHash = pHash[:]
		swap.State = SWAPSTATE_OPENING_TX_PREPARED
		err = s.store.Update(s.ctx, swap)
		if err != nil {
			return err
		}
		txId, err := s.CreateOpeningTransaction(ctx, swap)
		if err != nil {
			return err
		}
		swap.OpeningTxId = txId
		swap.State = SWAPSTATE_OPENING_TX_BROADCASTED
		err = s.store.Update(s.ctx, swap)
		if err != nil {
			return err
		}
		response := &MakerResponse{
			SwapId:          swap.Id,
			MakerPubkeyHash: swap.MakerPubkeyHash,
			Invoice:         payreq,
			TxId:            swap.OpeningTxId,
		}
		err = s.pc.SendMessage(swap.PeerNodeId, response)
		if err != nil {
			return err
		}
	} else if request.Type == SWAPTYPE_IN {

	}
	return nil
}

// CreateOpeningTransaction creates and broadcasts the opening Transaction,
// the two peers are the taker(pays the invoice) and the maker
func (s *Service) CreateOpeningTransaction(ctx context.Context, swap *Swap) (string, error) {
	// get the maker pubkey and privkey
	makerPubkey, err := s.wallet.GetPubkey()
	if err != nil {
		return "", err
	}
	makerPrivkey, err := s.wallet.GetPrivKey()
	if err != nil {
		return "", err
	}

	// Get the Inputs
	txInputs, change, err := s.wallet.GetUtxos(swap.Amount + FIXED_FEE)
	if err != nil {
		return "", err
	}

	// Outputs
	// Fees
	feeOutput, err := liquid.GetFeeOutput(FIXED_FEE)
	if err != nil {
		return "", err
	}

	// Change
	p2pkh := payment.FromPublicKey(makerPubkey, &network.Regtest, nil)
	changeScript := p2pkh.Script
	changeValue, err := elementsutil.SatoshiToElementsValue(change)
	if err != nil {
		return "", err
	}
	changeOutput := transaction.NewTxOutput(s.asset, changeValue[:], changeScript)

	// Swap
	// calc cltv

	blockHeight, err := s.blockchain.GetBlockHeight()
	if err != nil {
		return "", err
	}
	spendingBlockHeight := int64(blockHeight + LOCKTIME)

	takerPubkeyHashBytes, err := hex.DecodeString(swap.TakerPubkeyHash)
	if err != nil {
		return "", err
	}
	makerPubkeyHashBytes, err := hex.DecodeString(swap.MakerPubkeyHash)
	if err != nil {
		return "", err
	}
	redeemScript, err := liquid.GetOpeningTxScript(takerPubkeyHashBytes, makerPubkeyHashBytes, swap.PHash[:], spendingBlockHeight)
	if err != nil {
		return "", err
	}
	redeemPayment, err := payment.FromPayment(&payment.Payment{
		Script:  redeemScript,
		Network: s.network,
	})
	if err != nil {
		return "", err
	}

	swapInValue, err := elementsutil.SatoshiToElementsValue(swap.Amount)
	if err != nil {
		return "", err
	}

	redeemOutput := transaction.NewTxOutput(s.asset, swapInValue, redeemPayment.WitnessScript)

	// Create a new pset
	inputs,err := s.blockchain.WalletUtxosToTxInputs(txInputs)
	if err != nil {
		return "", err
	}
	outputs := []*transaction.TxOutput{redeemOutput, changeOutput, feeOutput}
	p, err := pset.New(inputs, outputs, 2, 0)
	if err != nil {
		return "", err
	}

	// Add sighash type and witness utxo to the partial input.
	updater, err := pset.NewUpdater(p)
	if err != nil {
		return "", err
	}

	bobspendingTxHash, err := s.blockchain.FetchTxHex(b2h(inputs[0].Hash))
	if err != nil {
		return "", err
	}
	bobFaucetTx, err := transaction.NewTxFromHex(bobspendingTxHash)
	if err != nil {
		return "", err
	}

	err = updater.AddInNonWitnessUtxo(bobFaucetTx, 0)
	if err != nil {
		return "", err
	}

	prvKeys := []*btcec.PrivateKey{makerPrivkey}
	scripts := [][]byte{p2pkh.Script}
	if err := liquid.SignTransaction(p, prvKeys, scripts, false, nil); err != nil {
		return "", err
	}

	// Finalize the partial transaction.
	if err := pset.FinalizeAll(p); err != nil {
		return "", err
	}
	// Extract the final signed transaction from the Pset wrapper.
	finalTx, err := pset.Extract(p)
	if err != nil {
		return "", err
	}
	// Serialize the transaction and try to broadcast.
	txHex, err := finalTx.ToHex()
	if err != nil {
		return "", err
	}
	txId, err := s.blockchain.BroadcastTransaction(txHex)
	if err != nil {
		return "", err
	}
	return txId, nil
}

func (s *Service) OnMakerResponse(senderNodeId string, request MakerResponse) error {
	swap, err := s.store.GetById(s.ctx, request.SwapId)
	if err != nil {
		return err
	}
	if swap.PeerNodeId != senderNodeId {
		return errors.New("peer has changed, aborting")
	}
	swap.State = SWAPSTATE_WAITING_FOR_TX
	swap.MakerPubkeyHash = request.MakerPubkeyHash
	swap.Payreq = request.Invoice
	swap.OpeningTxId = request.TxId

	invoice, err := s.lightning.DecodePayreq(swap.Payreq)
	if err != nil {
		return err
	}

	swap.PHash = invoice.PHash

	if invoice.Amount > swap.Amount+FIXED_FEE {
		return errors.New("invoice amount is to high")
	}

	err = s.store.Update(s.ctx, swap)
	if err != nil {
		return err
	}
	s.Mutex.Lock()
	s.txWatchList[swap.Id] = swap.OpeningTxId
	s.Mutex.Unlock()
	return nil
}
func (s *Service) ClaimTxWithPreimage(ctx context.Context, swap *Swap, tx *transaction.Transaction) error {
	err := s.CheckTransaction(ctx, swap, tx)
	if err != nil {
		return err
	}
	preimage, err := s.lightning.PayInvoice(swap.Payreq)
	if err != nil {
		return err
	}
	script, err := s.getSwapScript(swap)
	if err != nil {
		return err
	}

	// get the maker pubkey and privkey
	pubkey, err := s.wallet.GetPubkey()
	if err != nil {
		return err
	}
	privkey, err := s.wallet.GetPrivKey()
	if err != nil {
		return err
	}
	// Change
	p2pkh := payment.FromPublicKey(pubkey, &network.Regtest, nil)

	// second transaction
	firstTxHash := tx.WitnessHash()
	spendingInput := transaction.NewTxInput(firstTxHash[:], 0)
	firstTxSats, err := elementsutil.ElementsToSatoshiValue(tx.Outputs[0].Value)
	if err != nil {
		return err
	}
	spendingSatsBytes, err := elementsutil.SatoshiToElementsValue(firstTxSats - FIXED_FEE)
	if err != nil {
		return err
	}
	spendingOutput := transaction.NewTxOutput(s.asset, spendingSatsBytes[:], p2pkh.Script)

	feeOutput, err := liquid.GetFeeOutput(FIXED_FEE)
	if err != nil {
		return err
	}

	spendingTx := &transaction.Transaction{
		Version:  2,
		Flag:     0,
		Locktime: 0,
		Inputs:   []*transaction.TxInput{spendingInput},
		Outputs:  []*transaction.TxOutput{spendingOutput, feeOutput},
	}

	var sigHash [32]byte

	sigHash = spendingTx.HashForWitnessV0(
		0,
		script[:],
		tx.Outputs[0].Value,
		txscript.SigHashAll,
	)

	sig, err := privkey.Sign(sigHash[:])
	if err != nil {
		return err
	}
	sigWithHashType := append(sig.Serialize(), byte(txscript.SigHashAll))
	witness := make([][]byte, 0)
	preImageBytes, err := hex.DecodeString(preimage)
	if err != nil {
		return err
	}
	witness = append(witness, preImageBytes[:])
	witness = append(witness, sigWithHashType[:])
	witness = append(witness, script)
	spendingTx.Inputs[0].Witness = witness

	spendingTxHex, err := spendingTx.ToHex()
	if err != nil {
		return err
	}

	claimId, err := s.blockchain.BroadcastTransaction(spendingTxHex)
	if err != nil {
		return err
	}
	swap.ClaimTxId = claimId
	swap.State = SWAPSTATE_CLAIMED_PREIMAGE

	err = s.store.Update(s.ctx, swap)
	if err != nil {
		return err
	}
	return nil
}

// CheckTransaction checks if the opening transaction is according to the takers terms
func (s *Service) CheckTransaction(ctx context.Context, swap *Swap, tx *transaction.Transaction) error {

	script, err := s.getSwapScript(swap)
	if err != nil {
		return err
	}

	if bytes.Compare(tx.Outputs[0].Script, script) != 0 {
		return errors.New("tx script does not match computed script")
	}

	// check value
	value, err := elementsutil.SatoshiToElementsValue(swap.Amount)
	if err != nil {
		return err
	}
	if bytes.Compare(tx.Outputs[0].Value, value) != 0 {
		return errors.New("tx value does not match contract")
	}
	return nil
}

func (s *Service) getSwapScript(swap *Swap) ([]byte, error) {
	// check script
	takerPubkeyHashBytes, err := hex.DecodeString(swap.TakerPubkeyHash)
	if err != nil {
		return nil, err
	}
	makerPubkeyHashBytes, err := hex.DecodeString(swap.MakerPubkeyHash)
	if err != nil {
		return nil, err
	}
	script, err := liquid.GetOpeningTxScript(makerPubkeyHashBytes, takerPubkeyHashBytes, swap.PHash, LOCKTIME)
	if err != nil {
		return nil, err
	}
	return script, nil
}
func b2h(buf []byte) string {
	return hex.EncodeToString(buf)
}