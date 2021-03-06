// Copyright 2014 The go-avalanria Authors
// This file is part of the go-avalanria library.
//
// The go-avalanria library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-avalanria library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-avalanria library. If not, see <http://www.gnu.org/licenses/>.

// Package avn implements the Avalanria protocol.
package avn

import (
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avalanria/go-avalanria/accounts"
	"github.com/avalanria/go-avalanria/common"
	"github.com/avalanria/go-avalanria/common/hexutil"
	"github.com/avalanria/go-avalanria/consensus"
	"github.com/avalanria/go-avalanria/consensus/clique"
	"github.com/avalanria/go-avalanria/core"
	"github.com/avalanria/go-avalanria/core/bloombits"
	"github.com/avalanria/go-avalanria/core/rawdb"
	"github.com/avalanria/go-avalanria/core/state/pruner"
	"github.com/avalanria/go-avalanria/core/types"
	"github.com/avalanria/go-avalanria/core/vm"
	"github.com/avalanria/go-avalanria/avn/downloader"
	"github.com/avalanria/go-avalanria/avn/avnconfig"
	"github.com/avalanria/go-avalanria/avn/filters"
	"github.com/avalanria/go-avalanria/avn/gasprice"
	"github.com/avalanria/go-avalanria/avn/protocols/avn"
	"github.com/avalanria/go-avalanria/avn/protocols/snap"
	"github.com/avalanria/go-avalanria/avndb"
	"github.com/avalanria/go-avalanria/event"
	"github.com/avalanria/go-avalanria/internal/avnapi"
	"github.com/avalanria/go-avalanria/log"
	"github.com/avalanria/go-avalanria/miner"
	"github.com/avalanria/go-avalanria/node"
	"github.com/avalanria/go-avalanria/p2p"
	"github.com/avalanria/go-avalanria/p2p/dnsdisc"
	"github.com/avalanria/go-avalanria/p2p/enode"
	"github.com/avalanria/go-avalanria/params"
	"github.com/avalanria/go-avalanria/rlp"
	"github.com/avalanria/go-avalanria/rpc"
)

// Config contains the configuration options of the AVN protocol.
// Deprecated: use avnconfig.Config instead.
type Config = avnconfig.Config

// Avalanria implements the Avalanria full node service.
type Avalanria struct {
	config *avnconfig.Config

	// Handlers
	txPool             *core.TxPool
	blockchain         *core.BlockChain
	handler            *handler
	avnDialCandidates  enode.Iterator
	snapDialCandidates enode.Iterator

	// DB interfaces
	chainDb avndb.Database // Block chain database

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager

	bloomRequests     chan chan *bloombits.Retrieval // Channel receiving bloom data retrieval requests
	bloomIndexer      *core.ChainIndexer             // Bloom indexer operating during block imports
	closeBloomHandler chan struct{}

	APIBackend *EthAPIBackend

	miner     *miner.Miner
	gasPrice  *big.Int
	avnerbase common.Address

	networkID     uint64
	netRPCService *avnapi.PublicNetAPI

	p2pServer *p2p.Server

	lock sync.RWMutex // Protects the variadic fields (e.g. gas price and avnerbase)
}

// New creates a new Avalanria object (including the
// initialisation of the common Avalanria object)
func New(stack *node.Node, config *avnconfig.Config) (*Avalanria, error) {
	// Ensure configuration values are compatible and sane
	if config.SyncMode == downloader.LightSync {
		return nil, errors.New("can't run avn.Avalanria in light sync mode, use les.LightAvalanria")
	}
	if !config.SyncMode.IsValid() {
		return nil, fmt.Errorf("invalid sync mode %d", config.SyncMode)
	}
	if config.Miner.GasPrice == nil || config.Miner.GasPrice.Cmp(common.Big0) <= 0 {
		log.Warn("Sanitizing invalid miner gas price", "provided", config.Miner.GasPrice, "updated", avnconfig.Defaults.Miner.GasPrice)
		config.Miner.GasPrice = new(big.Int).Set(avnconfig.Defaults.Miner.GasPrice)
	}
	if config.NoPruning && config.TrieDirtyCache > 0 {
		if config.SnapshotCache > 0 {
			config.TrieCleanCache += config.TrieDirtyCache * 3 / 5
			config.SnapshotCache += config.TrieDirtyCache * 2 / 5
		} else {
			config.TrieCleanCache += config.TrieDirtyCache
		}
		config.TrieDirtyCache = 0
	}
	log.Info("Allocated trie memory caches", "clean", common.StorageSize(config.TrieCleanCache)*1024*1024, "dirty", common.StorageSize(config.TrieDirtyCache)*1024*1024)

	// Transfer mining-related config to the avnash config.
	avnashConfig := config.Ethash
	avnashConfig.NotifyFull = config.Miner.NotifyFull

	// Assemble the Avalanria object
	chainDb, err := stack.OpenDatabaseWithFreezer("chaindata", config.DatabaseCache, config.DatabaseHandles, config.DatabaseFreezer, "avn/db/chaindata/", false)
	if err != nil {
		return nil, err
	}
	chainConfig, genesisHash, genesisErr := core.SetupGenesisBlockWithOverride(chainDb, config.Genesis, config.OverrideLondon)
	if _, ok := genesisErr.(*params.ConfigCompatError); genesisErr != nil && !ok {
		return nil, genesisErr
	}
	log.Info("Initialised chain configuration", "config", chainConfig)

	if err := pruner.RecoverPruning(stack.ResolvePath(""), chainDb, stack.ResolvePath(config.TrieCleanCacheJournal)); err != nil {
		log.Error("Failed to recover state", "error", err)
	}
	avn := &Avalanria{
		config:            config,
		chainDb:           chainDb,
		eventMux:          stack.EventMux(),
		accountManager:    stack.AccountManager(),
		engine:            avnconfig.CreateConsensusEngine(stack, chainConfig, &avnashConfig, config.Miner.Notify, config.Miner.Noverify, chainDb),
		closeBloomHandler: make(chan struct{}),
		networkID:         config.NetworkId,
		gasPrice:          config.Miner.GasPrice,
		avnerbase:         config.Miner.Etherbase,
		bloomRequests:     make(chan chan *bloombits.Retrieval),
		bloomIndexer:      core.NewBloomIndexer(chainDb, params.BloomBitsBlocks, params.BloomConfirms),
		p2pServer:         stack.Server(),
	}

	bcVersion := rawdb.ReadDatabaseVersion(chainDb)
	var dbVer = "<nil>"
	if bcVersion != nil {
		dbVer = fmt.Sprintf("%d", *bcVersion)
	}
	log.Info("Initialising Avalanria protocol", "network", config.NetworkId, "dbversion", dbVer)

	if !config.SkipBcVersionCheck {
		if bcVersion != nil && *bcVersion > core.BlockChainVersion {
			return nil, fmt.Errorf("database version is v%d, Gavn %s only supports v%d", *bcVersion, params.VersionWithMeta, core.BlockChainVersion)
		} else if bcVersion == nil || *bcVersion < core.BlockChainVersion {
			if bcVersion != nil { // only print warning on upgrade, not on init
				log.Warn("Upgrade blockchain database version", "from", dbVer, "to", core.BlockChainVersion)
			}
			rawdb.WriteDatabaseVersion(chainDb, core.BlockChainVersion)
		}
	}
	var (
		vmConfig = vm.Config{
			EnablePreimageRecording: config.EnablePreimageRecording,
		}
		cacheConfig = &core.CacheConfig{
			TrieCleanLimit:      config.TrieCleanCache,
			TrieCleanJournal:    stack.ResolvePath(config.TrieCleanCacheJournal),
			TrieCleanRejournal:  config.TrieCleanCacheRejournal,
			TrieCleanNoPrefetch: config.NoPrefetch,
			TrieDirtyLimit:      config.TrieDirtyCache,
			TrieDirtyDisabled:   config.NoPruning,
			TrieTimeLimit:       config.TrieTimeout,
			SnapshotLimit:       config.SnapshotCache,
			Preimages:           config.Preimages,
		}
	)
	avn.blockchain, err = core.NewBlockChain(chainDb, cacheConfig, chainConfig, avn.engine, vmConfig, avn.shouldPreserve, &config.TxLookupLimit)
	if err != nil {
		return nil, err
	}
	// Rewind the chain in case of an incompatible config upgrade.
	if compat, ok := genesisErr.(*params.ConfigCompatError); ok {
		log.Warn("Rewinding chain to upgrade configuration", "err", compat)
		avn.blockchain.SetHead(compat.RewindTo)
		rawdb.WriteChainConfig(chainDb, genesisHash, chainConfig)
	}
	avn.bloomIndexer.Start(avn.blockchain)

	if config.TxPool.Journal != "" {
		config.TxPool.Journal = stack.ResolvePath(config.TxPool.Journal)
	}
	avn.txPool = core.NewTxPool(config.TxPool, chainConfig, avn.blockchain)

	// Permit the downloader to use the trie cache allowance during fast sync
	cacheLimit := cacheConfig.TrieCleanLimit + cacheConfig.TrieDirtyLimit + cacheConfig.SnapshotLimit
	checkpoint := config.Checkpoint
	if checkpoint == nil {
		checkpoint = params.TrustedCheckpoints[genesisHash]
	}
	if avn.handler, err = newHandler(&handlerConfig{
		Database:   chainDb,
		Chain:      avn.blockchain,
		TxPool:     avn.txPool,
		Network:    config.NetworkId,
		Sync:       config.SyncMode,
		BloomCache: uint64(cacheLimit),
		EventMux:   avn.eventMux,
		Checkpoint: checkpoint,
		Whitelist:  config.Whitelist,
	}); err != nil {
		return nil, err
	}

	avn.miner = miner.New(avn, &config.Miner, chainConfig, avn.EventMux(), avn.engine, avn.isLocalBlock)
	avn.miner.SetExtra(makeExtraData(config.Miner.ExtraData))

	avn.APIBackend = &EthAPIBackend{stack.Config().ExtRPCEnabled(), stack.Config().AllowUnprotectedTxs, avn, nil}
	if avn.APIBackend.allowUnprotectedTxs {
		log.Info("Unprotected transactions allowed")
	}
	gpoParams := config.GPO
	if gpoParams.Default == nil {
		gpoParams.Default = config.Miner.GasPrice
	}
	avn.APIBackend.gpo = gasprice.NewOracle(avn.APIBackend, gpoParams)

	// Setup DNS discovery iterators.
	dnsclient := dnsdisc.NewClient(dnsdisc.Config{})
	avn.avnDialCandidates, err = dnsclient.NewIterator(avn.config.EthDiscoveryURLs...)
	if err != nil {
		return nil, err
	}
	avn.snapDialCandidates, err = dnsclient.NewIterator(avn.config.SnapDiscoveryURLs...)
	if err != nil {
		return nil, err
	}

	// Start the RPC service
	avn.netRPCService = avnapi.NewPublicNetAPI(avn.p2pServer, config.NetworkId)

	// Register the backend on the node
	stack.RegisterAPIs(avn.APIs())
	stack.RegisterProtocols(avn.Protocols())
	stack.RegisterLifecycle(avn)
	// Check for unclean shutdown
	if uncleanShutdowns, discards, err := rawdb.PushUncleanShutdownMarker(chainDb); err != nil {
		log.Error("Could not update unclean-shutdown-marker list", "error", err)
	} else {
		if discards > 0 {
			log.Warn("Old unclean shutdowns found", "count", discards)
		}
		for _, tstamp := range uncleanShutdowns {
			t := time.Unix(int64(tstamp), 0)
			log.Warn("Unclean shutdown detected", "booted", t,
				"age", common.PrettyAge(t))
		}
	}
	return avn, nil
}

func makeExtraData(extra []byte) []byte {
	if len(extra) == 0 {
		// create default extradata
		extra, _ = rlp.EncodeToBytes([]interface{}{
			uint(params.VersionMajor<<16 | params.VersionMinor<<8 | params.VersionPatch),
			"gavn",
			runtime.Version(),
			runtime.GOOS,
		})
	}
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		log.Warn("Miner extra data exceed limit", "extra", hexutil.Bytes(extra), "limit", params.MaximumExtraDataSize)
		extra = nil
	}
	return extra
}

// APIs return the collection of RPC services the avalanria package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *Avalanria) APIs() []rpc.API {
	apis := avnapi.GetAPIs(s.APIBackend)

	// Append any APIs exposed explicitly by the consensus engine
	apis = append(apis, s.engine.APIs(s.BlockChain())...)

	// Append all the local APIs and return
	return append(apis, []rpc.API{
		{
			Namespace: "avn",
			Version:   "1.0",
			Service:   NewPublicAvalanriaAPI(s),
			Public:    true,
		}, {
			Namespace: "avn",
			Version:   "1.0",
			Service:   NewPublicMinerAPI(s),
			Public:    true,
		}, {
			Namespace: "avn",
			Version:   "1.0",
			Service:   downloader.NewPublicDownloaderAPI(s.handler.downloader, s.eventMux),
			Public:    true,
		}, {
			Namespace: "miner",
			Version:   "1.0",
			Service:   NewPrivateMinerAPI(s),
			Public:    false,
		}, {
			Namespace: "avn",
			Version:   "1.0",
			Service:   filters.NewPublicFilterAPI(s.APIBackend, false, 5*time.Minute),
			Public:    true,
		}, {
			Namespace: "admin",
			Version:   "1.0",
			Service:   NewPrivateAdminAPI(s),
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPublicDebugAPI(s),
			Public:    true,
		}, {
			Namespace: "debug",
			Version:   "1.0",
			Service:   NewPrivateDebugAPI(s),
		}, {
			Namespace: "net",
			Version:   "1.0",
			Service:   s.netRPCService,
			Public:    true,
		},
	}...)
}

func (s *Avalanria) ResetWithGenesisBlock(gb *types.Block) {
	s.blockchain.ResetWithGenesisBlock(gb)
}

func (s *Avalanria) Etherbase() (eb common.Address, err error) {
	s.lock.RLock()
	avnerbase := s.avnerbase
	s.lock.RUnlock()

	if avnerbase != (common.Address{}) {
		return avnerbase, nil
	}
	if wallets := s.AccountManager().Wallets(); len(wallets) > 0 {
		if accounts := wallets[0].Accounts(); len(accounts) > 0 {
			avnerbase := accounts[0].Address

			s.lock.Lock()
			s.avnerbase = avnerbase
			s.lock.Unlock()

			log.Info("Etherbase automatically configured", "address", avnerbase)
			return avnerbase, nil
		}
	}
	return common.Address{}, fmt.Errorf("avnerbase must be explicitly specified")
}

// isLocalBlock checks whavner the specified block is mined
// by local miner accounts.
//
// We regard two types of accounts as local miner account: avnerbase
// and accounts specified via `txpool.locals` flag.
func (s *Avalanria) isLocalBlock(block *types.Block) bool {
	author, err := s.engine.Author(block.Header())
	if err != nil {
		log.Warn("Failed to retrieve block author", "number", block.NumberU64(), "hash", block.Hash(), "err", err)
		return false
	}
	// Check whavner the given address is avnerbase.
	s.lock.RLock()
	avnerbase := s.avnerbase
	s.lock.RUnlock()
	if author == avnerbase {
		return true
	}
	// Check whavner the given address is specified by `txpool.local`
	// CLI flag.
	for _, account := range s.config.TxPool.Locals {
		if account == author {
			return true
		}
	}
	return false
}

// shouldPreserve checks whavner we should preserve the given block
// during the chain reorg depending on whavner the author of block
// is a local account.
func (s *Avalanria) shouldPreserve(block *types.Block) bool {
	// The reason we need to disable the self-reorg preserving for clique
	// is it can be probable to introduce a deadlock.
	//
	// e.g. If there are 7 available signers
	//
	// r1   A
	// r2     B
	// r3       C
	// r4         D
	// r5   A      [X] F G
	// r6    [X]
	//
	// In the round5, the inturn signer E is offline, so the worst case
	// is A, F and G sign the block of round5 and reject the block of opponents
	// and in the round6, the last available signer B is offline, the whole
	// network is stuck.
	if _, ok := s.engine.(*clique.Clique); ok {
		return false
	}
	return s.isLocalBlock(block)
}

// SetEtherbase sets the mining reward address.
func (s *Avalanria) SetEtherbase(avnerbase common.Address) {
	s.lock.Lock()
	s.avnerbase = avnerbase
	s.lock.Unlock()

	s.miner.SetEtherbase(avnerbase)
}

// StartMining starts the miner with the given number of CPU threads. If mining
// is already running, this mavnod adjust the number of threads allowed to use
// and updates the minimum price required by the transaction pool.
func (s *Avalanria) StartMining(threads int) error {
	// Update the thread count within the consensus engine
	type threaded interface {
		SetThreads(threads int)
	}
	if th, ok := s.engine.(threaded); ok {
		log.Info("Updated mining threads", "threads", threads)
		if threads == 0 {
			threads = -1 // Disable the miner from within
		}
		th.SetThreads(threads)
	}
	// If the miner was not running, initialize it
	if !s.IsMining() {
		// Propagate the initial price point to the transaction pool
		s.lock.RLock()
		price := s.gasPrice
		s.lock.RUnlock()
		s.txPool.SetGasPrice(price)

		// Configure the local mining address
		eb, err := s.Etherbase()
		if err != nil {
			log.Error("Cannot start mining without avnerbase", "err", err)
			return fmt.Errorf("avnerbase missing: %v", err)
		}
		if clique, ok := s.engine.(*clique.Clique); ok {
			wallet, err := s.accountManager.Find(accounts.Account{Address: eb})
			if wallet == nil || err != nil {
				log.Error("Etherbase account unavailable locally", "err", err)
				return fmt.Errorf("signer missing: %v", err)
			}
			clique.Authorize(eb, wallet.SignData)
		}
		// If mining is started, we can disable the transaction rejection mechanism
		// introduced to speed sync times.
		atomic.StoreUint32(&s.handler.acceptTxs, 1)

		go s.miner.Start(eb)
	}
	return nil
}

// StopMining terminates the miner, both at the consensus engine level as well as
// at the block creation level.
func (s *Avalanria) StopMining() {
	// Update the thread count within the consensus engine
	type threaded interface {
		SetThreads(threads int)
	}
	if th, ok := s.engine.(threaded); ok {
		th.SetThreads(-1)
	}
	// Stop the block creating itself
	s.miner.Stop()
}

func (s *Avalanria) IsMining() bool      { return s.miner.Mining() }
func (s *Avalanria) Miner() *miner.Miner { return s.miner }

func (s *Avalanria) AccountManager() *accounts.Manager  { return s.accountManager }
func (s *Avalanria) BlockChain() *core.BlockChain       { return s.blockchain }
func (s *Avalanria) TxPool() *core.TxPool               { return s.txPool }
func (s *Avalanria) EventMux() *event.TypeMux           { return s.eventMux }
func (s *Avalanria) Engine() consensus.Engine           { return s.engine }
func (s *Avalanria) ChainDb() avndb.Database            { return s.chainDb }
func (s *Avalanria) IsListening() bool                  { return true } // Always listening
func (s *Avalanria) Downloader() *downloader.Downloader { return s.handler.downloader }
func (s *Avalanria) Synced() bool                       { return atomic.LoadUint32(&s.handler.acceptTxs) == 1 }
func (s *Avalanria) ArchiveMode() bool                  { return s.config.NoPruning }
func (s *Avalanria) BloomIndexer() *core.ChainIndexer   { return s.bloomIndexer }

// Protocols returns all the currently configured
// network protocols to start.
func (s *Avalanria) Protocols() []p2p.Protocol {
	protos := avn.MakeProtocols((*avnHandler)(s.handler), s.networkID, s.avnDialCandidates)
	if s.config.SnapshotCache > 0 {
		protos = append(protos, snap.MakeProtocols((*snapHandler)(s.handler), s.snapDialCandidates)...)
	}
	return protos
}

// Start implements node.Lifecycle, starting all internal goroutines needed by the
// Avalanria protocol implementation.
func (s *Avalanria) Start() error {
	avn.StartENRUpdater(s.blockchain, s.p2pServer.LocalNode())

	// Start the bloom bits servicing goroutines
	s.startBloomHandlers(params.BloomBitsBlocks)

	// Figure out a max peers count based on the server limits
	maxPeers := s.p2pServer.MaxPeers
	if s.config.LightServ > 0 {
		if s.config.LightPeers >= s.p2pServer.MaxPeers {
			return fmt.Errorf("invalid peer config: light peer count (%d) >= total peer count (%d)", s.config.LightPeers, s.p2pServer.MaxPeers)
		}
		maxPeers -= s.config.LightPeers
	}
	// Start the networking layer and the light server if requested
	s.handler.Start(maxPeers)
	return nil
}

// Stop implements node.Lifecycle, terminating all internal goroutines used by the
// Avalanria protocol.
func (s *Avalanria) Stop() error {
	// Stop all the peer-related stuff first.
	s.avnDialCandidates.Close()
	s.snapDialCandidates.Close()
	s.handler.Stop()

	// Then stop everything else.
	s.bloomIndexer.Close()
	close(s.closeBloomHandler)
	s.txPool.Stop()
	s.miner.Stop()
	s.blockchain.Stop()
	s.engine.Close()
	rawdb.PopUncleanShutdownMarker(s.chainDb)
	s.chainDb.Close()
	s.eventMux.Stop()

	return nil
}
