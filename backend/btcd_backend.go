package backend

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/pkg/errors"
	"github.com/square/beancounter/deriver"
	"github.com/square/beancounter/reporter"
	"github.com/square/beancounter/utils"
)

// BtcdBackend wraps Btcd node and its API to provide a simple
// balance and transaction history information for a given address.
// BtcdBackend implements Backend interface.
type BtcdBackend struct {
	client            *rpcclient.Client
	network           utils.Network
	maxBlockHeight    int64
	blockHeightMu     sync.Mutex // mutex to guard read/writes to blockHeightLookup map
	blockHeightLookup map[string]int64

	// channels used to communicate with the Accounter
	addrRequests  chan *deriver.Address
	addrResponses chan *AddrResponse
	txRequests    chan string
	txResponses   chan *TxResponse

	// internal channels
	transactionsMu     sync.Mutex // mutex to guard read/writes to transactions map
	cachedTransactions map[string]*TxResponse
	doneCh             chan bool
}

const (
	// min number of confirmations required
	// any blocks with lower confirmation numbers will be ignored
	minConfirmations = 6
	// For now assume that there cannot be more than maxTxsPerAddr.
	// Ideally, if maxTxsPerAddr is reached then we should paginate and retrieve
	// all the transactions.
	maxTxsPerAddr = 1000

	addrRequestsChanSize = 100

	concurrency = 100
)

// NewBtcdBackend returns a new BtcdBackend structs or errors.
// BtcdBackend takes into account maxBlockHeight and ignores any transactions that belong to higher blocks.
// If 0 is passed, then the block chain is queried for max block height and minConfirmations is subtracted
// (to avoid querying blocks that might potentially be orphaned).
//
// NOTE: BtcdBackend is assumed to be connecting to a personal node, hence it disables TLS for now
func NewBtcdBackend(maxBlockHeight int64, hostPort, user, pass string, network utils.Network) (*BtcdBackend, error) {
	connCfg := &rpcclient.ConnConfig{
		Host:         hostPort,
		User:         user,
		Pass:         pass,
		HTTPPostMode: true, // Bitcoin core only supports HTTP POST mode
		DisableTLS:   true, // Since we're assuming a personal bitcoin node for now, skip TLS
	}
	client, err := rpcclient.New(connCfg, nil)
	if err != nil {
		return nil, errors.Wrap(err, "could not create a Btcd RPC client")
	}

	actualMaxHeight, err := client.GetBlockCount()
	maxAllowedHeight := actualMaxHeight - minConfirmations
	if err != nil {
		return nil, errors.Wrap(err, "could not connect to the Btcd server")
	}
	if maxBlockHeight == 0 {
		maxBlockHeight = maxAllowedHeight
	}
	if maxAllowedHeight < maxBlockHeight {
		return nil, fmt.Errorf("wanted max block height: %d, block chain has %d (with # confirmations of %d)", maxBlockHeight, maxAllowedHeight, minConfirmations)
	}

	b := &BtcdBackend{
		client:             client,
		network:            network,
		maxBlockHeight:     maxBlockHeight,
		addrRequests:       make(chan *deriver.Address, addrRequestsChanSize),
		addrResponses:      make(chan *AddrResponse, addrRequestsChanSize),
		txRequests:         make(chan string, 2*maxTxsPerAddr),
		txResponses:        make(chan *TxResponse, 2*maxTxsPerAddr),
		blockHeightLookup:  make(map[string]int64),
		cachedTransactions: make(map[string]*TxResponse),
		doneCh:             make(chan bool),
	}

	// launch
	for i := 0; i < concurrency; i++ {
		go b.processRequests()
	}
	return b, nil
}

// AddrRequest schedules a request to the backend to lookup information related
// to the given address.
func (b *BtcdBackend) AddrRequest(addr *deriver.Address) {
	reporter.GetInstance().IncAddressesScheduled()
	reporter.GetInstance().Log(fmt.Sprintf("scheduling address: %s", addr))
	b.addrRequests <- addr
}

// AddrResponses exposes a channel that allows to consume backend's responses to
// address requests created with AddrRequest()
func (b *BtcdBackend) AddrResponses() <-chan *AddrResponse {
	return b.addrResponses
}

// TxRequest schedules a request to the backend to lookup information related
// to the given transaction hash.
func (b *BtcdBackend) TxRequest(txHash string) {
	reporter.GetInstance().IncTxScheduled()
	reporter.GetInstance().Log(fmt.Sprintf("scheduling tx: %s", txHash))
	b.txRequests <- txHash
}

// TxResponses exposes a channel that allows to consume backend's responses to
// address requests created with addrrequest().
// if an address has any transactions then they will be sent to this channel by the
// backend.
func (b *BtcdBackend) TxResponses() <-chan *TxResponse {
	return b.txResponses
}

// Finish informs the backend to stop doing its work.
func (b *BtcdBackend) Finish() {
	close(b.doneCh)
	b.client.Disconnect()
}

func (b *BtcdBackend) processRequests() {
	for {
		select {
		case addr := <-b.addrRequests:
			err := b.processAddrRequest(addr)
			if err != nil {
				panic(fmt.Sprintf("processAddrRequest failed: %+v", err))
			}
		case tx := <-b.txRequests:
			err := b.processTxRequest(tx)
			if err != nil {
				panic(fmt.Sprintf("processTxRequest failed: %+v", err))
			}
		case <-b.doneCh:
			break
		}
	}
}

func (b *BtcdBackend) processAddrRequest(address *deriver.Address) error {
	addr := address.Script()
	txs, err := b.client.SearchRawTransactionsVerbose(address.Address(), 0, maxTxsPerAddr+1, true, false, nil)
	if err != nil {
		if jerr, ok := err.(*btcjson.RPCError); ok {
			switch jerr.Code {
			case btcjson.ErrRPCInvalidAddressOrKey:
				// the address doesn't exist in the blockchain - either because it was not used
				// or given backend doesn't have a complete blockchain
				b.addrResponses <- &AddrResponse{
					Address:  address,
					TxHashes: []string{},
				}
				return nil
			}
		}
		return errors.Wrap(err, "could not fetch transactions for "+addr)
	}

	if len(txs) > maxTxsPerAddr {
		return fmt.Errorf("address %s has more than max allowed transactions of %d", addr, maxTxsPerAddr)
	}

	txHashes := make([]string, 0, len(txs))
	for _, tx := range txs {
		txHashes = append(txHashes, tx.Txid)
	}

	b.cacheTxs(txs)

	b.addrResponses <- &AddrResponse{
		Address:  address,
		TxHashes: txHashes,
	}

	return nil
}

func (b *BtcdBackend) processTxRequest(txHash string) error {
	b.transactionsMu.Lock()
	tx, exists := b.cachedTransactions[txHash]
	b.transactionsMu.Unlock()

	if exists {
		b.txResponses <- tx

		return nil
	}

	hash, err := chainhash.NewHashFromStr(txHash)
	if err != nil {
		return err
	}
	txResp, err := b.client.GetRawTransactionVerbose(hash)
	if err != nil {
		if jerr, ok := err.(*btcjson.RPCError); ok {
			switch jerr.Code {
			case btcjson.ErrRPCInvalidAddressOrKey:
				return errors.Wrap(err, "blockchain doesn't have transaction "+txHash)
			}
		}
		return errors.Wrap(err, "could not fetch transaction "+txHash)
	}
	height, err := b.getBlockHeight(txResp.BlockHash)
	if err != nil {
		return err
	}

	b.txResponses <- &TxResponse{
		Hash:   txHash,
		Height: height,
		Hex:    txResp.Hex,
	}
	return nil
}

func (b *BtcdBackend) cacheTxs(txs []*btcjson.SearchRawTransactionsResult) {
	for _, tx := range txs {
		b.transactionsMu.Lock()
		_, exists := b.cachedTransactions[tx.Txid]
		b.transactionsMu.Unlock()

		if exists {
			return
		}

		height, err := b.getBlockHeight(tx.BlockHash)
		if err != nil {
			panic(fmt.Sprintf("error getting block height for hash %s: %s", tx.BlockHash, err.Error()))
		}

		b.transactionsMu.Lock()
		b.cachedTransactions[tx.Txid] = &TxResponse{
			Hash:   tx.Txid,
			Height: height,
			Hex:    tx.Hex,
		}
		b.transactionsMu.Unlock()
	}
}

// getBlockHeight returns a block height for a given block hash or returns an error
func (b *BtcdBackend) getBlockHeight(hash string) (int64, error) {
	b.blockHeightMu.Lock()
	height, exists := b.blockHeightLookup[hash]
	b.blockHeightMu.Unlock()
	if exists {
		return height, nil
	}

	h, err := chainhash.NewHashFromStr(hash)
	if err != nil {
		return -1, err
	}
	resp, err := b.client.GetBlockVerbose(h)
	if err != nil {
		if jerr, ok := err.(*btcjson.RPCError); ok {
			switch jerr.Code {
			case btcjson.ErrRPCInvalidAddressOrKey:
				return -1, errors.Wrap(err, "blockchain doesn't have block "+hash)
			}
		}
		return -1, errors.Wrap(err, "could not fetch block "+hash)
	}

	b.blockHeightMu.Lock()
	b.blockHeightLookup[hash] = resp.Height
	b.blockHeightMu.Unlock()

	return resp.Height, nil
}
