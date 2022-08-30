package core

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/spruce-solutions/go-quai/common"
	"github.com/spruce-solutions/go-quai/consensus"
	"github.com/spruce-solutions/go-quai/core/rawdb"
	"github.com/spruce-solutions/go-quai/core/types"
	"github.com/spruce-solutions/go-quai/core/vm"
	"github.com/spruce-solutions/go-quai/ethclient/quaiclient"
	"github.com/spruce-solutions/go-quai/ethdb"
	"github.com/spruce-solutions/go-quai/log"
	"github.com/spruce-solutions/go-quai/params"
)

const (
	maxFutureBlocks     = 256
	maxTimeFutureBlocks = 30
	pendingHeaderLimit  = 10
)

type Slice struct {
	hc *HeaderChain

	txPool *TxPool
	miner  *Miner

	sliceDb ethdb.Database
	config  *params.ChainConfig
	engine  consensus.Engine

	quit chan struct{} // slice quit channel

	domClient  *quaiclient.Client
	domUrl     string
	subClients []*quaiclient.Client

	futureBlocks *lru.Cache

	appendmu sync.RWMutex

	nilHeader        *types.Header
	nilPendingHeader types.PendingHeader

	wg sync.WaitGroup // slice processing wait group for shutting down

	pendingHeader types.PendingHeader
	phCache       map[uint64][]types.PendingHeader
}

func NewSlice(db ethdb.Database, config *Config, txConfig *TxPoolConfig, isLocalBlock func(block *types.Header) bool, chainConfig *params.ChainConfig, domClientUrl string, subClientUrls []string, engine consensus.Engine, cacheConfig *CacheConfig, vmConfig vm.Config) (*Slice, error) {
	sl := &Slice{
		config:  chainConfig,
		engine:  engine,
		sliceDb: db,
		domUrl:  domClientUrl,
	}

	futureBlocks, _ := lru.New(maxFutureBlocks)
	sl.futureBlocks = futureBlocks

	var err error
	sl.hc, err = NewHeaderChain(db, engine, chainConfig, cacheConfig, vmConfig)
	if err != nil {
		return nil, err
	}

	sl.txPool = NewTxPool(*txConfig, chainConfig, sl.hc)
	sl.miner = New(sl.hc, sl.txPool, config, db, chainConfig, engine, isLocalBlock)
	sl.miner.SetExtra(sl.miner.MakeExtraData(config.ExtraData))

	// only set the subClients if the chain is not Zone
	sl.subClients = make([]*quaiclient.Client, 3)
	if types.QuaiNetworkContext != params.ZONE {
		sl.subClients = MakeSubClients(subClientUrls)
	}

	domDoneCh := make(chan struct{})
	// only set domClient if the chain is not Prime.
	if types.QuaiNetworkContext != params.PRIME {
		go func(done chan struct{}) {
			sl.domClient = MakeDomClient(domClientUrl)
			done <- struct{}{}
		}(domDoneCh)
	}

	sl.nilHeader = &types.Header{
		ParentHash:        make([]common.Hash, 3),
		Number:            make([]*big.Int, 3),
		Extra:             make([][]byte, 3),
		Time:              uint64(0),
		BaseFee:           make([]*big.Int, 3),
		GasLimit:          make([]uint64, 3),
		Coinbase:          make([]common.Address, 3),
		Difficulty:        make([]*big.Int, 3),
		NetworkDifficulty: make([]*big.Int, 3),
		Root:              make([]common.Hash, 3),
		TxHash:            make([]common.Hash, 3),
		UncleHash:         make([]common.Hash, 3),
		ReceiptHash:       make([]common.Hash, 3),
		GasUsed:           make([]uint64, 3),
		Bloom:             make([]types.Bloom, 3),
	}
	sl.nilPendingHeader = types.PendingHeader{
		Header:  sl.nilHeader,
		Termini: make([]common.Hash, 3),
		Td:      big.NewInt(0),
	}

	var pendingHeader *types.Header

	// load the pending header
	pendingHeader = rawdb.ReadPendingHeader(sl.sliceDb, sl.hc.CurrentHeader().Parent())
	if pendingHeader == nil {
		// Update the pending header to the genesis Header.
		pendingHeader = sl.hc.genesisHeader
	}

	go sl.updateFutureBlocks()
	if types.QuaiNetworkContext == params.ZONE {
		go sl.domDoneLoop(domDoneCh, pendingHeader)
	}

	return sl, nil
}

func (sl *Slice) SliceAppend(block *types.Block, td *big.Int, domReorg bool, currentContextOrigin bool) error {
	// Append the block
	PendingHeader, err := sl.Append(block, common.Hash{}, big.NewInt(0), false, true)
	if err != nil {
		return err
	}

	bestPendingHeader := sl.sortAndGetBestPendingHeader(sl.phCache, PendingHeader)
}

func (sl *Slice) Append(block *types.Block, domTerminus common.Hash, td *big.Int, domReorg bool, currentContextOrigin bool) (types.PendingHeader, error) {
	sl.appendmu.Lock()
	defer sl.appendmu.Unlock()

	//PCRC
	domTerminus, err := sl.PCRC(block.Header(), domTerminus)
	if err != nil {
		return sl.nilPendingHeader, err
	}

	// Append the new block
	err = sl.hc.Append(block)
	if err != nil {
		fmt.Println("Slice error in append", err)
		return sl.nilPendingHeader, err
	}

	if currentContextOrigin {
		// CalcTd on the new block
		td, err = sl.CalcTd(block.Header())
		if err != nil {
			return sl.nilPendingHeader, err
		}
	}

	sl.pendingHeader, err = sl.setHeaderChainHead(block.Header(), td, domReorg, currentContextOrigin)
	tempPendingHeader := types.CopyHeader(sl.pendingHeader.Header)
	if err != nil {
		return sl.nilPendingHeader, err
	}
	// WriteTd
	// Remove this once td is converted to a single value.
	externTd := make([]*big.Int, 3)
	externTd[types.QuaiNetworkContext] = td
	rawdb.WriteTd(sl.hc.headerDb, block.Header().Hash(), block.NumberU64(), externTd)

	if types.QuaiNetworkContext != params.ZONE {
		// Perform the sub append
		subPendingHeader, err := sl.subClients[block.Header().Location[types.QuaiNetworkContext]-1].Append(context.Background(), block, domTerminus, td, domReorg, false)
		if err != nil {
			return sl.nilPendingHeader, err
		}
		tempPendingHeader = subPendingHeader.Header
		tempPendingHeader = sl.combinePendingHeader(sl.pendingHeader.Header, tempPendingHeader, types.QuaiNetworkContext+1)
	}

	if types.QuaiNetworkContext == params.PRIME {
		//save the pending header
		rawdb.WritePendingHeader(sl.sliceDb, block.Hash(), tempPendingHeader)

		//transmit it to the miner
		sl.miner.worker.pendingHeaderFeed.Send(tempPendingHeader)
	} else {
		sl.domClient.SendPendingHeader(context.Background(), tempPendingHeader, domTerminus)
	}
	return types.PendingHeader{Header: tempPendingHeader, Termini: sl.pendingHeader.Termini, Td: sl.pendingHeader.Td}, nil
}

func (sl *Slice) setHeaderChainHead(head *types.Header, td *big.Int, domReorg bool, currentContextOrigin bool) (types.PendingHeader, error) {

	if currentContextOrigin {
		reorg := sl.HLCR(td)
		if reorg {
			_, err := sl.hc.SetCurrentHeader(head)
			if err != nil {
				return sl.nilPendingHeader, err
			}
		}
	} else {
		if domReorg {
			_, err := sl.hc.SetCurrentHeader(head)
			if err != nil {
				return sl.nilPendingHeader, err
			}
		}
	}

	// Upate the local pending header
	slPendingHeader, err := sl.miner.worker.GeneratePendingHeader(head)
	if err != nil {
		fmt.Println("pending block error: ", err)
		return sl.nilPendingHeader, err
	}
	if types.QuaiNetworkContext == params.ZONE {
		slPendingHeader.Location = sl.config.Location
		slPendingHeader.Time = uint64(time.Now().Unix())
	}

	termini := rawdb.ReadTermini(sl.sliceDb, head.Hash())

	return types.PendingHeader{Header: slPendingHeader, Termini: termini, Td: td}, nil
}

// PCRC
func (sl *Slice) PCRC(header *types.Header, domTerminus common.Hash) (common.Hash, error) {
	termini := sl.hc.GetTerminiByHash(header.Parent())

	if termini == nil {
		return common.Hash{}, consensus.ErrFutureBlock
	}

	newTermini := termini

	var nilHash common.Hash
	// make sure the termini match
	if domTerminus != nilHash {
		// There is a dom block so we must check the terminuses match
		if termini[len(termini)-1] != domTerminus {
			return common.Hash{}, errors.New("termini do not match, block rejected due to twist with dom")
		} else {
			newTermini[sl.config.Location[types.QuaiNetworkContext]-1] = header.Hash()
		}

		// Update the terminus for the block
		parentHeader := sl.hc.GetHeaderByHash(header.Parent())
		parentOrder, err := sl.engine.GetDifficultyOrder(parentHeader)
		if err != nil {
			return common.Hash{}, err
		}
		if parentOrder < types.QuaiNetworkContext {
			newTermini[len(newTermini)-1] = header.Parent()
		}

		//Save the termini
		rawdb.WriteTermini(sl.hc.headerDb, header.Hash(), newTermini)
	}

	return termini[sl.config.Location[types.QuaiNetworkContext]-1], nil
}

// HLCR
func (sl *Slice) HLCR(externTd *big.Int) bool {
	currentTd := sl.hc.GetTdByHash(sl.hc.CurrentHeader().Hash())
	return currentTd[types.QuaiNetworkContext].Cmp(externTd) < 0
}

// CalcTd calculates the TD of the given header using PCRC and CalcHLCRNetDifficulty.
func (sl *Slice) CalcTd(header *types.Header) (*big.Int, error) {
	priorTd := sl.hc.GetTd(header.Parent(), header.Number64()-1)
	if priorTd[types.QuaiNetworkContext] == nil {
		return nil, consensus.ErrFutureBlock
	}
	Td := priorTd[types.QuaiNetworkContext].Add(priorTd[types.QuaiNetworkContext], header.Difficulty[types.QuaiNetworkContext])
	return Td, nil
}

// writePendingHeader updates the slice pending header at the given index with the value from given header.
func (sl *Slice) combinePendingHeader(header *types.Header, slPendingHeader *types.Header, index int) *types.Header {
	slPendingHeader.ParentHash[index] = header.ParentHash[index]
	slPendingHeader.UncleHash[index] = header.UncleHash[index]
	slPendingHeader.Number[index] = header.Number[index]
	slPendingHeader.Extra[index] = header.Extra[index]
	slPendingHeader.BaseFee[index] = header.BaseFee[index]
	slPendingHeader.GasLimit[index] = header.GasLimit[index]
	slPendingHeader.GasUsed[index] = header.GasUsed[index]
	slPendingHeader.TxHash[index] = header.TxHash[index]
	slPendingHeader.ReceiptHash[index] = header.ReceiptHash[index]
	slPendingHeader.Root[index] = header.Root[index]
	slPendingHeader.Difficulty[index] = header.Difficulty[index]
	slPendingHeader.Coinbase[index] = header.Coinbase[index]
	slPendingHeader.Bloom[index] = header.Bloom[index]

	return slPendingHeader
}

// sortAndGetBestPendingHeader takes in a phCache, and a pendingHeader. Filters though the cache
// with location in int as a key and returns the pendingHeader with best Total Difficulty.
func (sl *Slice) sortAndGetBestPendingHeader(phCache map[uint64][]types.PendingHeader, pendingHeader types.PendingHeader) types.PendingHeader {
	location := pendingHeader.Header.Location
	// convert location in bytes to int to use as the key
	key := binary.BigEndian.Uint64(location)
	pendingHeaders := phCache[key]
	pendingHeaders = append(pendingHeaders, pendingHeader)

	sort.Slice(pendingHeaders, func(i, j int) bool {
		return pendingHeaders[i].Td.Cmp(pendingHeaders[j].Td) < 0
	})

	if len(pendingHeaders) > pendingHeaderLimit {
		pendingHeaders[0] = sl.nilPendingHeader
		pendingHeaders = pendingHeaders[1:]
	}
	phCache[key] = pendingHeaders

	return pendingHeaders[len(pendingHeaders)-1]
}

// ReceivePendingHeader receives a pendingHeader from the subs and if the order of the block
// is less than the context of the chain, the pendingHeader is sent to the dom.
func (sl *Slice) ReceivePendingHeader(slPendingHeader *types.Header, terminusHash common.Hash) error {

	if sl.pendingHeader.Termini[slPendingHeader.Location[types.QuaiNetworkContext]-1] != terminusHash {
		log.Info("Stale update received from sub")
		return nil
	}

	pendingHeader := sl.pendingHeader.Header
	fmt.Println("ReceivePendingHeader pendingHeader", pendingHeader)
	fmt.Println("ReceivePendingHeader slPendingHeader", slPendingHeader)
	slPendingHeader = sl.combinePendingHeader(pendingHeader, slPendingHeader, types.QuaiNetworkContext)

	if types.QuaiNetworkContext == params.PRIME {
		if slPendingHeader.ParentHash[types.QuaiNetworkContext] == sl.config.GenesisHashes[types.QuaiNetworkContext] {
			time.Sleep(10 * time.Second)
		}
		sl.miner.worker.pendingHeaderFeed.Send(slPendingHeader)
	} else {
		if slPendingHeader.ParentHash[types.QuaiNetworkContext] == sl.config.GenesisHashes[types.QuaiNetworkContext] {
			fmt.Println("RecievPendingHeader slPendingHeader", slPendingHeader)
			domDoneCh := make(chan struct{})
			// only set domClient if the chain is not Prime.
			if types.QuaiNetworkContext != params.PRIME {
				go func(done chan struct{}) {
					sl.domClient = MakeDomClient(sl.domUrl)
					done <- struct{}{}
				}(domDoneCh)
			}
			go sl.domDoneLoop(domDoneCh, slPendingHeader)

		} else {
			sl.domClient.SendPendingHeader(context.Background(), slPendingHeader, sl.pendingHeader.Termini[3])
		}

	}
	return nil
}

// MakeDomClient creates the quaiclient for the given domurl
func MakeDomClient(domurl string) *quaiclient.Client {
	if domurl == "" {
		log.Crit("dom client url is empty")
	}
	domClient, err := quaiclient.Dial(domurl)
	if err != nil {
		log.Crit("Error connecting to the dominant go-quai client", "err", err)
	}
	return domClient
}

// MakeSubClients creates the quaiclient for the given suburls
func MakeSubClients(suburls []string) []*quaiclient.Client {
	subClients := make([]*quaiclient.Client, 3)
	for i, suburl := range suburls {
		if suburl == "" {
			log.Warn("sub client url is empty")
		}
		subClient, err := quaiclient.Dial(suburl)
		if err != nil {
			log.Crit("Error connecting to the subordinate go-quai client for index", "index", i, " err ", err)
		}
		subClients[i] = subClient
	}
	return subClients
}

func (sl *Slice) procFutureBlocks() {
	blocks := make([]*types.Block, 0, sl.futureBlocks.Len())
	for _, hash := range sl.futureBlocks.Keys() {
		if block, exist := sl.futureBlocks.Peek(hash); exist {
			blocks = append(blocks, block.(*types.Block))
		}
	}
	if len(blocks) > 0 {
		sort.Slice(blocks, func(i, j int) bool {
			return blocks[i].NumberU64() < blocks[j].NumberU64()
		})
		// Insert one by one as chain insertion needs contiguous ancestry between blocks
		for i := range blocks {
			fmt.Println("blocks in future blocks", blocks[i].Header().Number, blocks[i].Header().Hash())
		}
		// Insert one by one as chain insertion needs contiguous ancestry between blocks
		for i := range blocks {
			var nilHash common.Hash
			sl.Append(blocks[i], nilHash, big.NewInt(0), false, true)
		}
	}
}

func (sl *Slice) addFutureBlock(block *types.Block) error {
	max := uint64(time.Now().Unix() + maxTimeFutureBlocks)
	if block.Time() > max {
		return fmt.Errorf("future block timestamp %v > allowed %v", block.Time(), max)
	}
	if !sl.futureBlocks.Contains(block.Hash()) {
		sl.futureBlocks.Add(block.Hash(), block)
	}
	return nil
}

func (sl *Slice) updateFutureBlocks() {
	futureTimer := time.NewTicker(3 * time.Second)
	defer futureTimer.Stop()
	defer sl.wg.Done()
	for {
		select {
		case <-futureTimer.C:
			sl.procFutureBlocks()
		case <-sl.quit:
			return
		}
	}
}

// domDoneLoop
func (sl *Slice) domDoneLoop(domDone chan struct{}, pendingHeader *types.Header) error {
	for {
		select {
		case <-domDone:
			if types.QuaiNetworkContext == params.ZONE {
				sl.pendingHeader, _ = sl.setHeaderChainHead(pendingHeader, sl.hc.GetTdByHash(pendingHeader.Hash())[types.QuaiNetworkContext], true, false)
				fmt.Println("tempPendingHeader on start: ", sl.pendingHeader.Header)

				sl.domClient.SendPendingHeader(context.Background(), sl.pendingHeader.Header, sl.config.GenesisHashes[types.QuaiNetworkContext])
			}
			if types.QuaiNetworkContext == params.REGION {
				sl.domClient.SendPendingHeader(context.Background(), sl.pendingHeader.Header, sl.config.GenesisHashes[types.QuaiNetworkContext])
			}
		}
	}
}

func (sl *Slice) GetSliceHeadHash(index byte) common.Hash { return common.Hash{} }

func (sl *Slice) GetHeadHash() common.Hash { return sl.hc.currentHeaderHash }

func (sl *Slice) Config() *params.ChainConfig { return sl.config }

func (sl *Slice) Engine() consensus.Engine { return sl.engine }

func (sl *Slice) HeaderChain() *HeaderChain { return sl.hc }

func (sl *Slice) TxPool() *TxPool { return sl.txPool }

func (sl *Slice) Miner() *Miner { return sl.miner }

func (sl *Slice) PendingBlockBody(hash common.Hash) *types.Body {
	return rawdb.ReadPendginBlockBody(sl.sliceDb, hash)
}
