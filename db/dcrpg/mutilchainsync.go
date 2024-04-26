// Copyright (c) 2018-2021, The Decred developers
// Copyright (c) 2017, The dcrdata developers
// See LICENSE for details.

package dcrpg

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	btcchaincfg "github.com/btcsuite/btcd/chaincfg"
	btcClient "github.com/btcsuite/btcd/rpcclient"
	btcwire "github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrdata/v8/db/dbtypes"
	"github.com/decred/dcrdata/v8/mutilchain"
	"github.com/decred/dcrdata/v8/mutilchain/btcrpcutils"
	"github.com/decred/dcrdata/v8/mutilchain/ltcrpcutils"
	"github.com/decred/dcrdata/v8/mutilchain/mutilchainrpcutils"
	"github.com/decred/dcrdata/v8/mutilchain/structure"
	"github.com/ltcsuite/ltcd/chaincfg"
	ltcClient "github.com/ltcsuite/ltcd/rpcclient"
	"github.com/ltcsuite/ltcd/wire"
	"github.com/ybbus/jsonrpc"
)

// SyncChainDBAsync is like SyncChainDB except it also takes a result channel on
// which the caller should wait to receive the result. As such, this method
// should be called as a goroutine or it will hang on send if the channel is
// unbuffered.
func (db *ChainDB) SyncLTCChainDBAsync(res chan dbtypes.SyncResult,
	client *ltcClient.Client, quit chan struct{}, newIndexes, updateAllAddresses bool) {
	if db == nil {
		res <- dbtypes.SyncResult{
			Height: -1,
			Error:  fmt.Errorf("ChainDB LTC (psql) disabled"),
		}
		return
	}
	height, err := db.SyncLTCChainDB(client, quit, newIndexes, updateAllAddresses)
	res <- dbtypes.SyncResult{
		Height: height,
		Error:  err,
	}
}

func (db *ChainDB) SyncBTCChainDBAsync(res chan dbtypes.SyncResult,
	client *btcClient.Client, quit chan struct{}, newIndexes, updateAllAddresses bool) {
	if db == nil {
		res <- dbtypes.SyncResult{
			Height: -1,
			Error:  fmt.Errorf("ChainDB BTC (psql) disabled"),
		}
		return
	}
	height, err := db.SyncBTCChainDB(client, quit, newIndexes, updateAllAddresses)
	res <- dbtypes.SyncResult{
		Height: height,
		Error:  err,
	}
}

func (db *ChainDB) SyncMutilchainDBAsync(res chan dbtypes.SyncResult, client jsonrpc.RPCClient, quit chan struct{}, newIndexes, updateAllAddresses bool, chainType string) {
	if db == nil {
		res <- dbtypes.SyncResult{
			Height: -1,
			Error:  fmt.Errorf("ChainDB LTC (psql) disabled"),
		}
		return
	}
	height, err := db.SyncMutilchainChainDB(client, quit, newIndexes, updateAllAddresses, chainType)
	res <- dbtypes.SyncResult{
		Height: height,
		Error:  err,
	}
}

func (db *ChainDB) SyncMutilchainChainDB(client jsonrpc.RPCClient, quit chan struct{},
	newIndexes, updateAllAddresses bool, chainType string) (int64, error) {
	// Get chain servers's best block
	_, nodeHeight, err := mutilchainrpcutils.GetBestBlock(client, chainType)
	if err != nil {
		return -1, fmt.Errorf("GetBestBlock %s failed: %v", chainType, err)
	}
	// Total and rate statistics
	var totalTxs, totalVins, totalVouts int64
	var lastTxs, lastVins, lastVouts int64
	tickTime := 20 * time.Second
	ticker := time.NewTicker(tickTime)
	startTime := time.Now()
	o := sync.Once{}
	speedReporter := func() {
		ticker.Stop()
		totalElapsed := time.Since(startTime).Seconds()
		if int64(totalElapsed) == 0 {
			return
		}
		totalVoutPerSec := totalVouts / int64(totalElapsed)
		totalTxPerSec := totalTxs / int64(totalElapsed)
		log.Infof("Avg. speed: %d tx/s, %d vout/s", totalTxPerSec, totalVoutPerSec)
	}
	speedReport := func() { o.Do(speedReporter) }
	defer speedReport()

	startingHeight, err := db.MutilchainHeightDB(chainType)
	lastBlock := int64(startingHeight)
	if err != nil {
		if err == sql.ErrNoRows {
			lastBlock = -1
			log.Info("blocks table is empty, starting fresh.")
		} else {
			return -1, fmt.Errorf("RetrieveBestBlockHeight: %v", err)
		}
	}

	// Remove indexes/constraints before bulk import
	blocksToSync := int64(nodeHeight) - lastBlock
	reindexing := newIndexes || blocksToSync > int64(nodeHeight)/2
	if reindexing {
		log.Info("Mutilchain: Large bulk load: Removing indexes and disabling duplicate checks.", chainType)
		err = db.DeindexAllMutilchain(chainType)
		if err != nil && !strings.Contains(err.Error(), "does not exist") && !strings.Contains(err.Error(), "不存在") {
			return lastBlock, err
		}
		db.MutilchainEnableDuplicateCheckOnInsert(false, chainType)
	} else {
		db.MutilchainEnableDuplicateCheckOnInsert(true, chainType)
	}

	// Start rebuilding
	startHeight := lastBlock + 1
	for ib := startHeight; ib <= int64(nodeHeight); ib++ {
		// check for quit signal
		select {
		case <-quit:
			log.Infof("Mutilchain %s: Rescan cancelled at height %d.", chainType, ib)
			return ib - 1, nil
		default:
		}

		if (ib-1)%mutilchainRescanLogBlockChunk == 0 || ib == startHeight {
			if ib == 0 {
				log.Infof("Mutilchain %s: Scanning genesis block.", chainType)
			} else {
				endRangeBlock := mutilchainRescanLogBlockChunk * (1 + (ib-1)/mutilchainRescanLogBlockChunk)
				if endRangeBlock > int64(nodeHeight) {
					endRangeBlock = int64(nodeHeight)
				}
				log.Infof("Mutilchain %s: Processing blocks %d to %d...", chainType, ib, endRangeBlock)
			}
		}
		select {
		case <-ticker.C:
			blocksPerSec := float64(ib-lastBlock) / tickTime.Seconds()
			txPerSec := float64(totalTxs-lastTxs) / tickTime.Seconds()
			vinsPerSec := float64(totalVins-lastVins) / tickTime.Seconds()
			voutPerSec := float64(totalVouts-lastVouts) / tickTime.Seconds()
			log.Infof("(%3d blk/s,%5d tx/s,%5d vin/sec,%5d vout/s)", int64(blocksPerSec),
				int64(txPerSec), int64(vinsPerSec), int64(voutPerSec))
			lastBlock, lastTxs = ib, totalTxs
			lastVins, lastVouts = totalVins, totalVouts
		default:
		}
		//Get Block
		block, blockHash, err := mutilchainrpcutils.GetBlock(client, ib, chainType)
		if err != nil {
			return ib - 1, fmt.Errorf("%s: GetBlock failed (%s): %v", chainType, blockHash, err)
		}
		var numVins, numVouts int64
		if numVins, numVouts, err = db.StoreMutilchainBlock(client, block, true, updateAllAddresses, chainType); err != nil {
			return ib - 1, fmt.Errorf("%s: StoreBlock failed: %v", chainType, err)
		}
		totalVins += numVins
		totalVouts += numVouts

		numRTx := int64(len(block.Transactions))
		totalTxs += numRTx
		// totalRTxs += numRTx
		// totalSTxs += numSTx

		// update height, the end condition for the loop
		if _, nodeHeight, err = mutilchainrpcutils.GetBestBlock(client, chainType); err != nil {
			return ib, fmt.Errorf("%s: GetBestBlock failed: %v", chainType, err)
		}
	}

	speedReport()

	if reindexing || newIndexes {
		if err = db.IndexAllMutilchain(nil, chainType); err != nil {
			return int64(nodeHeight), fmt.Errorf("%s: IndexAllMutilchain failed: %v", chainType, err)
		}
		if !updateAllAddresses {
			err = db.IndexMutilchainAddressTable(nil, chainType)
		}
	}

	if updateAllAddresses {
		// Remove existing indexes not on funding txns
		_ = db.DeindexMutilchainAddressTable(chainType) // ignore errors for non-existent indexes
		log.Infof("%s: Populating spending tx info in address table...", chainType)
		numAddresses, err := db.UpdateMutilchainSpendingInfoInAllAddresses(chainType)
		if err != nil {
			log.Errorf("%s: UpdateSpendingInfoInAllAddresses for %s FAILED: %v", chainType, chainType, err)
		}
		log.Infof("%s: Updated %d rows of address table", chainType, numAddresses)
		if err = db.IndexMutilchainAddressTable(nil, chainType); err != nil {
			log.Errorf("%s: IndexBTCAddressTable FAILED: %v", chainType, err)
		}
	}
	db.MutilchainEnableDuplicateCheckOnInsert(true, chainType)
	log.Infof("%s: Sync finished at height %d. Delta: %d blocks, %d transactions, %d ins, %d outs", chainType,
		nodeHeight, int64(nodeHeight)-startHeight+1, totalTxs, totalVins, totalVouts)
	return int64(nodeHeight), err
}

func (db *ChainDB) SyncBTCChainDB(client *btcClient.Client, quit chan struct{},
	newIndexes, updateAllAddresses bool) (int64, error) {
	// Get chain servers's best block
	_, nodeHeight, err := client.GetBestBlock()
	if err != nil {
		return -1, fmt.Errorf("GetBestBlock BTC failed: %v", err)
	}
	// Total and rate statistics
	var totalTxs, totalVins, totalVouts int64
	var lastTxs, lastVins, lastVouts int64
	tickTime := 20 * time.Second
	ticker := time.NewTicker(tickTime)
	startTime := time.Now()
	o := sync.Once{}
	speedReporter := func() {
		ticker.Stop()
		totalElapsed := time.Since(startTime).Seconds()
		if int64(totalElapsed) == 0 {
			return
		}
		totalVoutPerSec := totalVouts / int64(totalElapsed)
		totalTxPerSec := totalTxs / int64(totalElapsed)
		log.Infof("Avg. speed: %d tx/s, %d vout/s", totalTxPerSec, totalVoutPerSec)
	}
	speedReport := func() { o.Do(speedReporter) }
	defer speedReport()

	startingHeight, err := db.MutilchainHeightDB(mutilchain.TYPEBTC)
	lastBlock := int64(startingHeight)
	if err != nil {
		if err == sql.ErrNoRows {
			lastBlock = -1
			log.Info("blocks table is empty, starting fresh.")
		} else {
			return -1, fmt.Errorf("RetrieveBestBlockHeight: %v", err)
		}
	}

	// Remove indexes/constraints before bulk import
	blocksToSync := int64(nodeHeight) - lastBlock
	reindexing := newIndexes || blocksToSync > int64(nodeHeight)/2
	if reindexing {
		log.Info("BTC Large bulk load: Removing indexes and disabling duplicate checks.")
		err = db.DeindexAllMutilchain(mutilchain.TYPEBTC)
		if err != nil && !strings.Contains(err.Error(), "does not exist") && !strings.Contains(err.Error(), "不存在") {
			return lastBlock, err
		}
		db.MutilchainEnableDuplicateCheckOnInsert(false, mutilchain.TYPEBTC)
	} else {
		db.MutilchainEnableDuplicateCheckOnInsert(true, mutilchain.TYPEBTC)
	}

	// Start rebuilding
	startHeight := lastBlock + 1
	for ib := startHeight; ib <= int64(nodeHeight); ib++ {
		// check for quit signal
		select {
		case <-quit:
			log.Infof("BTC: Rescan cancelled at height %d.", ib)
			return ib - 1, nil
		default:
		}

		if (ib-1)%btcRescanLogBlockChunk == 0 || ib == startHeight {
			if ib == 0 {
				log.Infof("BTC: Scanning genesis block.")
			} else {
				endRangeBlock := btcRescanLogBlockChunk * (1 + (ib-1)/btcRescanLogBlockChunk)
				if endRangeBlock > int64(nodeHeight) {
					endRangeBlock = int64(nodeHeight)
				}
				log.Infof("BTC: Processing blocks %d to %d...", ib, endRangeBlock)
			}
		}
		select {
		case <-ticker.C:
			blocksPerSec := float64(ib-lastBlock) / tickTime.Seconds()
			txPerSec := float64(totalTxs-lastTxs) / tickTime.Seconds()
			vinsPerSec := float64(totalVins-lastVins) / tickTime.Seconds()
			voutPerSec := float64(totalVouts-lastVouts) / tickTime.Seconds()
			log.Infof("(%3d blk/s,%5d tx/s,%5d vin/sec,%5d vout/s)", int64(blocksPerSec),
				int64(txPerSec), int64(vinsPerSec), int64(voutPerSec))
			lastBlock, lastTxs = ib, totalTxs
			lastVins, lastVouts = totalVins, totalVouts
		default:
		}

		block, blockHash, err := btcrpcutils.GetBlock(ib, client)
		if err != nil {
			return ib - 1, fmt.Errorf("BTC: GetBlock failed (%s): %v", blockHash, err)
		}
		var numVins, numVouts int64
		if numVins, numVouts, err = db.StoreBTCBlock(client, block.MsgBlock(), true, !updateAllAddresses); err != nil {
			return ib - 1, fmt.Errorf("BTC: StoreBlock failed: %v", err)
		}
		totalVins += numVins
		totalVouts += numVouts

		numRTx := int64(len(block.Transactions()))
		totalTxs += numRTx
		// totalRTxs += numRTx
		// totalSTxs += numSTx

		// update height, the end condition for the loop
		if _, nodeHeight, err = client.GetBestBlock(); err != nil {
			return ib, fmt.Errorf("BTC: GetBestBlock failed: %v", err)
		}
	}

	speedReport()

	if reindexing || newIndexes {
		if err = db.IndexAllMutilchain(nil, mutilchain.TYPEBTC); err != nil {
			return int64(nodeHeight), fmt.Errorf("BTC: IndexAllMutilchain failed: %v", err)
		}
		if !updateAllAddresses {
			err = db.IndexMutilchainAddressTable(nil, mutilchain.TYPEBTC)
		}
	}

	if updateAllAddresses {
		// Remove existing indexes not on funding txns
		_ = db.DeindexMutilchainAddressTable(mutilchain.TYPEBTC) // ignore errors for non-existent indexes
		log.Infof("BTC: Populating spending tx info in address table...")
		numAddresses, err := db.UpdateMutilchainSpendingInfoInAllAddresses(mutilchain.TYPEBTC)
		if err != nil {
			log.Errorf("BTC: UpdateSpendingInfoInAllAddresses for BTC FAILED: %v", err)
		}
		log.Infof("BTC: Updated %d rows of address table", numAddresses)
		if err = db.IndexMutilchainAddressTable(nil, mutilchain.TYPEBTC); err != nil {
			log.Errorf("BTC: IndexBTCAddressTable FAILED: %v", err)
		}
	}
	db.MutilchainEnableDuplicateCheckOnInsert(true, mutilchain.TYPEBTC)
	log.Infof("BTC: Sync finished at height %d. Delta: %d blocks, %d transactions, %d ins, %d outs",
		nodeHeight, int64(nodeHeight)-startHeight+1, totalTxs, totalVins, totalVouts)
	return int64(nodeHeight), err
}

func (db *ChainDB) SyncLTCChainDB(client *ltcClient.Client, quit chan struct{},
	newIndexes, updateAllAddresses bool) (int64, error) {
	// Get chain servers's best block
	_, nodeHeight, err := client.GetBestBlock()
	if err != nil {
		return -1, fmt.Errorf("GetBestBlock LTC failed: %v", err)
	}
	// Total and rate statistics
	var totalTxs, totalVins, totalVouts int64
	var lastTxs, lastVins, lastVouts int64
	tickTime := 20 * time.Second
	ticker := time.NewTicker(tickTime)
	startTime := time.Now()
	o := sync.Once{}
	speedReporter := func() {
		ticker.Stop()
		totalElapsed := time.Since(startTime).Seconds()
		if int64(totalElapsed) == 0 {
			return
		}
		totalVoutPerSec := totalVouts / int64(totalElapsed)
		totalTxPerSec := totalTxs / int64(totalElapsed)
		log.Infof("Avg. speed: %d tx/s, %d vout/s", totalTxPerSec, totalVoutPerSec)
	}
	speedReport := func() { o.Do(speedReporter) }
	defer speedReport()

	startingHeight, err := db.MutilchainHeightDB(mutilchain.TYPELTC)
	lastBlock := int64(startingHeight)
	if err != nil {
		if err == sql.ErrNoRows {
			lastBlock = -1
			log.Info("blocks table is empty, starting fresh.")
		} else {
			return -1, fmt.Errorf("RetrieveBestBlockHeight: %v", err)
		}
	}

	// Remove indexes/constraints before bulk import
	blocksToSync := int64(nodeHeight) - lastBlock
	reindexing := newIndexes || blocksToSync > int64(nodeHeight)/2
	if reindexing {
		log.Info("Large bulk load: Removing indexes and disabling duplicate checks.")
		err = db.DeindexAllMutilchain(mutilchain.TYPELTC)
		if err != nil && !strings.Contains(err.Error(), "does not exist") && !strings.Contains(err.Error(), "不存在") {
			return lastBlock, err
		}
		db.MutilchainEnableDuplicateCheckOnInsert(false, mutilchain.TYPELTC)
	} else {
		db.MutilchainEnableDuplicateCheckOnInsert(true, mutilchain.TYPELTC)
	}

	// Start rebuilding
	startHeight := lastBlock + 1
	for ib := startHeight; ib <= int64(nodeHeight); ib++ {
		// check for quit signal
		select {
		case <-quit:
			log.Infof("Rescan cancelled at height %d.", ib)
			return ib - 1, nil
		default:
		}

		if (ib-1)%ltcRescanLogBlockChunk == 0 || ib == startHeight {
			if ib == 0 {
				log.Infof("Scanning genesis block.")
			} else {
				endRangeBlock := ltcRescanLogBlockChunk * (1 + (ib-1)/ltcRescanLogBlockChunk)
				if endRangeBlock > int64(nodeHeight) {
					endRangeBlock = int64(nodeHeight)
				}
				log.Infof("Processing blocks %d to %d...", ib, endRangeBlock)
			}
		}
		select {
		case <-ticker.C:
			blocksPerSec := float64(ib-lastBlock) / tickTime.Seconds()
			txPerSec := float64(totalTxs-lastTxs) / tickTime.Seconds()
			vinsPerSec := float64(totalVins-lastVins) / tickTime.Seconds()
			voutPerSec := float64(totalVouts-lastVouts) / tickTime.Seconds()
			log.Infof("(%3d blk/s,%5d tx/s,%5d vin/sec,%5d vout/s)", int64(blocksPerSec),
				int64(txPerSec), int64(vinsPerSec), int64(voutPerSec))
			lastBlock, lastTxs = ib, totalTxs
			lastVins, lastVouts = totalVins, totalVouts
		default:
		}

		block, blockHash, err := ltcrpcutils.GetBlock(ib, client)
		if err != nil {
			return ib - 1, fmt.Errorf("GetBlock failed (%s): %v", blockHash, err)
		}
		var numVins, numVouts int64
		if numVins, numVouts, err = db.StoreLTCBlock(client, block.MsgBlock(), true, !updateAllAddresses); err != nil {
			return ib - 1, fmt.Errorf("StoreBlock failed: %v", err)
		}
		totalVins += numVins
		totalVouts += numVouts

		numRTx := int64(len(block.Transactions()))
		totalTxs += numRTx
		// totalRTxs += numRTx
		// totalSTxs += numSTx

		// update height, the end condition for the loop
		if _, nodeHeight, err = client.GetBestBlock(); err != nil {
			return ib, fmt.Errorf("GetBestBlock failed: %v", err)
		}
	}

	speedReport()

	if reindexing || newIndexes {
		if err = db.IndexAllMutilchain(nil, mutilchain.TYPELTC); err != nil {
			return int64(nodeHeight), fmt.Errorf("IndexAllMutilchain failed: %v", err)
		}
		if !updateAllAddresses {
			err = db.IndexMutilchainAddressTable(nil, mutilchain.TYPELTC)
		}
	}

	if updateAllAddresses {
		// Remove existing indexes not on funding txns
		_ = db.DeindexMutilchainAddressTable(mutilchain.TYPELTC) // ignore errors for non-existent indexes
		log.Infof("Populating spending tx info in address table...")
		numAddresses, err := db.UpdateMutilchainSpendingInfoInAllAddresses(mutilchain.TYPELTC)
		if err != nil {
			log.Errorf("UpdateSpendingInfoInAllAddresses for LTC FAILED: %v", err)
		}
		log.Infof("Updated %d rows of address table", numAddresses)
		if err = db.IndexMutilchainAddressTable(nil, mutilchain.TYPELTC); err != nil {
			log.Errorf("IndexLTCAddressTable FAILED: %v", err)
		}
	}
	db.MutilchainEnableDuplicateCheckOnInsert(true, mutilchain.TYPELTC)
	log.Infof("Sync finished at height %d. Delta: %d blocks, %d transactions, %d ins, %d outs",
		nodeHeight, int64(nodeHeight)-startHeight+1, totalTxs, totalVins, totalVouts)

	return int64(nodeHeight), err
}

// StoreBlock processes the input wire.MsgBlock, and saves to the data tables.
// The number of vins, and vouts stored are also returned.
func (pgb *ChainDB) StoreBTCBlock(client *btcClient.Client, msgBlock *btcwire.MsgBlock,
	isValid, updateAddressesSpendingInfo bool) (numVins int64, numVouts int64, err error) {
	// Convert the wire.MsgBlock to a dbtypes.Block
	dbBlock := dbtypes.MsgBTCBlockToDBBlock(client, msgBlock, pgb.btcChainParams)
	// Extract transactions and their vouts, and insert vouts into their pg table,
	// returning their DB PKs, which are stored in the corresponding transaction
	// data struct. Insert each transaction once they are updated with their
	// vouts' IDs, returning the transaction PK ID, which are stored in the
	// containing block data struct.

	// regular transactions
	resChanReg := make(chan storeTxnsResult)
	go func() {
		resChanReg <- pgb.storeBTCTxns(client, dbBlock, msgBlock,
			pgb.btcChainParams, &dbBlock.TxDbIDs, updateAddressesSpendingInfo)
	}()
	errReg := <-resChanReg
	numVins = errReg.numVins
	numVouts = errReg.numVouts
	// Store the block now that it has all it's transaction PK IDs
	var blockDbID uint64
	blockDbID, err = InsertMutilchainBlock(pgb.db, dbBlock, isValid, pgb.btcDupChecks, mutilchain.TYPEBTC)
	if err != nil {
		log.Error("BTC: InsertBlock:", err)
		return
	}
	pgb.btcLastBlock[msgBlock.BlockHash()] = blockDbID

	pgb.BtcBestBlock = &MutilchainBestBlock{
		Height: int64(dbBlock.Height),
		Hash:   dbBlock.Hash,
	}
	err = InsertMutilchainBlockPrevNext(pgb.db, blockDbID, dbBlock.Hash,
		dbBlock.PreviousHash, "", mutilchain.TYPEBTC)
	if err != nil && err != sql.ErrNoRows {
		log.Error("BTC: InsertBlockPrevNext:", err)
		return
	}

	// Update last block in db with this block's hash as it's next. Also update
	// isValid flag in last block if votes in this block invalidated it.
	lastBlockHash := msgBlock.Header.PrevBlock
	lastBlockDbID, ok := pgb.btcLastBlock[lastBlockHash]
	if ok {
		log.Infof("BTC: Setting last block %s. Height: %d", lastBlockHash, dbBlock.Height)
		err = UpdateMutilchainLastBlock(pgb.db, lastBlockDbID, false, mutilchain.TYPEBTC)
		if err != nil {
			log.Error("BTC: UpdateLastBlock:", err)
			return
		}
		err = UpdateMutilchainBlockNext(pgb.db, lastBlockDbID, dbBlock.Hash, mutilchain.TYPEBTC)
		if err != nil {
			log.Error("UpdateBlockNext:", err)
			return
		}
	}
	return
}

func (pgb *ChainDB) SetMutilchainLastBlock(chainType string, hashString string, blockDBID uint64) {
	switch chainType {
	case mutilchain.TYPEDERO:
		pgb.deroLastBlock[hashString] = blockDBID
	default:
		return
	}
}

func (pgb *ChainDB) SetMutilchainBestBlock(chainType string, hash string, height int64) {
	switch chainType {
	case mutilchain.TYPEDERO:
		pgb.DeroBestBlock = &MutilchainBestBlock{
			Height: height,
			Hash:   hash,
		}
	default:
		return
	}
}

func (pgb *ChainDB) GetMutilchainLastBlockDBId(chainType string, hash string) (uint64, bool) {
	switch chainType {
	case mutilchain.TYPEDERO:
		id, ok := pgb.deroLastBlock[hash]
		return id, ok
	default:
		return 0, false
	}
}

func (pgb *ChainDB) StoreMutilchainBlock(client jsonrpc.RPCClient, block *structure.MsgBlock,
	isValid, updateAddressesSpendingInfo bool, chainType string) (numVins int64, numVouts int64, err error) {
	// Convert the wire.MsgBlock to a dbtypes.Block
	dbBlock := dbtypes.MsgMutilchainBlockToDBBlock(client, block)

	// regular transactions
	resChanReg := make(chan storeTxnsResult)
	go func() {
		resChanReg <- pgb.storeMutilchainTxns(client, dbBlock, block, chainType, &dbBlock.TxDbIDs, updateAddressesSpendingInfo)
	}()
	dupCheck := pgb.GetMutilchainDupcheck(chainType)
	errReg := <-resChanReg
	numVins = errReg.numVins
	numVouts = errReg.numVouts
	// Store the block now that it has all it's transaction PK IDs
	var blockDbID uint64
	blockDbID, err = InsertMutilchainBlock(pgb.db, dbBlock, isValid, dupCheck, chainType)
	if err != nil {
		log.Error("%s: InsertBlock:%v", chainType, err)
		return
	}
	pgb.SetMutilchainLastBlock(chainType, block.Hash, blockDbID)
	pgb.SetMutilchainBestBlock(chainType, dbBlock.Hash, int64(dbBlock.Height))
	err = InsertMutilchainBlockPrevNext(pgb.db, blockDbID, dbBlock.Hash,
		dbBlock.PreviousHash, "", chainType)
	if err != nil && err != sql.ErrNoRows {
		log.Error("%s: InsertBlockPrevNext: %v", chainType, err)
		return
	}

	// Update last block in db with this block's hash as it's next. Also update
	// isValid flag in last block if votes in this block invalidated it.
	lastBlockHash := block.PrevHash
	lastBlockDbID, ok := pgb.GetMutilchainLastBlockDBId(chainType, lastBlockHash)
	if ok {
		log.Infof("%s: Setting last block %s. Height: %d", chainType, lastBlockHash, dbBlock.Height)
		err = UpdateMutilchainLastBlock(pgb.db, lastBlockDbID, false, chainType)
		if err != nil {
			log.Error("%s: UpdateLastBlock:%v", chainType, err)
			return
		}
		err = UpdateMutilchainBlockNext(pgb.db, lastBlockDbID, dbBlock.Hash, chainType)
		if err != nil {
			log.Error("%s UpdateBlockNext:%v", chainType, err)
			return
		}
	}
	return
}

// StoreBlock processes the input wire.MsgBlock, and saves to the data tables.
// The number of vins, and vouts stored are also returned.
func (pgb *ChainDB) StoreLTCBlock(client *ltcClient.Client, msgBlock *wire.MsgBlock,
	isValid, updateAddressesSpendingInfo bool) (numVins int64, numVouts int64, err error) {
	// Convert the wire.MsgBlock to a dbtypes.Block
	dbBlock := dbtypes.MsgLTCBlockToDBBlock(client, msgBlock, pgb.ltcChainParams)
	// Extract transactions and their vouts, and insert vouts into their pg table,
	// returning their DB PKs, which are stored in the corresponding transaction
	// data struct. Insert each transaction once they are updated with their
	// vouts' IDs, returning the transaction PK ID, which are stored in the
	// containing block data struct.

	// regular transactions
	resChanReg := make(chan storeTxnsResult)
	go func() {
		resChanReg <- pgb.storeLTCTxns(client, dbBlock, msgBlock,
			pgb.ltcChainParams, &dbBlock.TxDbIDs, updateAddressesSpendingInfo)
	}()

	errReg := <-resChanReg
	numVins = errReg.numVins
	numVouts = errReg.numVouts

	// Store the block now that it has all it's transaction PK IDs
	var blockDbID uint64
	blockDbID, err = InsertMutilchainBlock(pgb.db, dbBlock, isValid, pgb.ltcDupChecks, mutilchain.TYPELTC)
	if err != nil {
		log.Error("InsertBlock:", err)
		return
	}
	pgb.ltcLastBlock[msgBlock.BlockHash()] = blockDbID

	pgb.LtcBestBlock = &MutilchainBestBlock{
		Height: int64(dbBlock.Height),
		Hash:   dbBlock.Hash,
	}

	err = InsertMutilchainBlockPrevNext(pgb.db, blockDbID, dbBlock.Hash,
		dbBlock.PreviousHash, "", mutilchain.TYPELTC)
	if err != nil && err != sql.ErrNoRows {
		log.Error("InsertBlockPrevNext:", err)
		return
	}

	// Update last block in db with this block's hash as it's next. Also update
	// isValid flag in last block if votes in this block invalidated it.
	lastBlockHash := msgBlock.Header.PrevBlock
	lastBlockDbID, ok := pgb.ltcLastBlock[lastBlockHash]
	if ok {
		log.Infof("Setting last block %s. Height: %d", lastBlockHash, dbBlock.Height)
		err = UpdateMutilchainLastBlock(pgb.db, lastBlockDbID, false, mutilchain.TYPELTC)
		if err != nil {
			log.Error("UpdateLastBlock:", err)
			return
		}
		err = UpdateMutilchainBlockNext(pgb.db, lastBlockDbID, dbBlock.Hash, mutilchain.TYPELTC)
		if err != nil {
			log.Error("UpdateBlockNext:", err)
			return
		}
	}
	return
}

func (pgb *ChainDB) storeLTCTxns(client *ltcClient.Client, block *dbtypes.Block, msgBlock *wire.MsgBlock,
	chainParams *chaincfg.Params, TxDbIDs *[]uint64,
	updateAddressesSpendingInfo bool) storeTxnsResult {
	dbTransactions, dbTxVouts, dbTxVins := dbtypes.ExtractLTCBlockTransactions(client, block,
		msgBlock, chainParams)
	var txRes storeTxnsResult
	dbAddressRows := make([][]dbtypes.MutilchainAddressRow, len(dbTransactions))
	var totalAddressRows int
	var err error
	for it, dbtx := range dbTransactions {
		dbtx.VoutDbIds, dbAddressRows[it], err = InsertMutilchainVouts(pgb.db, dbTxVouts[it], pgb.ltcDupChecks, mutilchain.TYPELTC)
		if err != nil && err != sql.ErrNoRows {
			log.Error("InsertVouts:", err)
			txRes.err = err
			return txRes
		}
		totalAddressRows += len(dbAddressRows[it])
		txRes.numVouts += int64(len(dbtx.VoutDbIds))
		if err == sql.ErrNoRows || len(dbTxVouts[it]) != len(dbtx.VoutDbIds) {
			log.Warnf("Incomplete Vout insert.")
		}

		dbtx.VinDbIds, err = InsertMutilchainVins(pgb.db, dbTxVins[it], mutilchain.TYPELTC, pgb.ltcDupChecks)
		if err != nil && err != sql.ErrNoRows {
			log.Error("InsertVins:", err)
			txRes.err = err
			return txRes
		}
		txRes.numVins += int64(len(dbtx.VinDbIds))
	}

	// Get the tx PK IDs for storage in the blocks table
	*TxDbIDs, err = InsertMutilchainTxns(pgb.db, dbTransactions, pgb.ltcDupChecks, mutilchain.TYPELTC)
	if err != nil && err != sql.ErrNoRows {
		log.Error("InsertTxns:", err)
		txRes.err = err
		return txRes
	}

	// Store tx Db IDs as funding tx in AddressRows and rearrange
	dbAddressRowsFlat := make([]*dbtypes.MutilchainAddressRow, 0, totalAddressRows)
	for it, txDbID := range *TxDbIDs {
		// Set the tx ID of the funding transactions
		for iv := range dbAddressRows[it] {
			// Transaction that pays to the address
			dba := &dbAddressRows[it][iv]
			dba.FundingTxDbID = txDbID
			// Funding tx hash, vout id, value, and address are already assigned
			// by InsertVouts. Only the funding tx DB ID was needed.
			dbAddressRowsFlat = append(dbAddressRowsFlat, dba)
		}
	}

	// Insert each new AddressRow, absent spending fields
	_, err = InsertMutilchainAddressOuts(pgb.db, dbAddressRowsFlat, mutilchain.TYPELTC, pgb.ltcDupChecks)
	if err != nil {
		log.Error("InsertAddressOuts:", err)
		txRes.err = err
		return txRes
	}

	if !updateAddressesSpendingInfo {
		return txRes
	}

	// Check the new vins and update sending tx data in Addresses table
	for it, txDbID := range *TxDbIDs {
		for iv := range dbTxVins[it] {
			// Transaction that spends an outpoint paying to >=0 addresses
			vin := &dbTxVins[it][iv]
			// Get the tx hash and vout index (previous output) from vins table
			// vinDbID, txHash, txIndex, _, err := RetrieveFundingOutpointByTxIn(
			// 	pgb.db, vin.TxID, vin.TxIndex)
			vinDbID := dbTransactions[it].VinDbIds[iv]
			// skip coinbase inputs
			if bytes.Equal(zeroHashStringBytes, []byte(vin.PrevTxHash)) {
				continue
			}

			var numAddressRowsSet int64
			numAddressRowsSet, err = SetMutilchainSpendingForFundingOP(pgb.db,
				vin.PrevTxHash, vin.PrevTxIndex, // funding
				txDbID, vin.TxID, vin.TxIndex, vinDbID, mutilchain.TYPELTC) // spending
			if err != nil {
				log.Errorf("SetSpendingForFundingOP: %v", err)
			}
			txRes.numAddresses += numAddressRowsSet
		}
	}

	return txRes
}

func (pgb *ChainDB) storeBTCTxns(client *btcClient.Client, block *dbtypes.Block, msgBlock *btcwire.MsgBlock,
	chainParams *btcchaincfg.Params, TxDbIDs *[]uint64,
	updateAddressesSpendingInfo bool) storeTxnsResult {
	dbTransactions, dbTxVouts, dbTxVins := dbtypes.ExtractBTCBlockTransactions(client, block,
		msgBlock, chainParams)
	var txRes storeTxnsResult
	dbAddressRows := make([][]dbtypes.MutilchainAddressRow, len(dbTransactions))
	var totalAddressRows int
	var err error
	for it, dbtx := range dbTransactions {
		dbtx.VoutDbIds, dbAddressRows[it], err = InsertMutilchainVouts(pgb.db, dbTxVouts[it], pgb.btcDupChecks, mutilchain.TYPEBTC)
		if err != nil && err != sql.ErrNoRows {
			log.Error("BTC: InsertVouts:", err)
			txRes.err = err
			return txRes
		}
		totalAddressRows += len(dbAddressRows[it])
		txRes.numVouts += int64(len(dbtx.VoutDbIds))
		if err == sql.ErrNoRows || len(dbTxVouts[it]) != len(dbtx.VoutDbIds) {
			log.Warnf("BTC: Incomplete Vout insert.")
		}
		dbtx.VinDbIds, err = InsertMutilchainVins(pgb.db, dbTxVins[it], mutilchain.TYPEBTC, pgb.btcDupChecks)
		if err != nil && err != sql.ErrNoRows {
			log.Error("BTC: InsertVins:", err)
			txRes.err = err
			return txRes
		}
		txRes.numVins += int64(len(dbtx.VinDbIds))
	}
	// Get the tx PK IDs for storage in the blocks table
	*TxDbIDs, err = InsertMutilchainTxns(pgb.db, dbTransactions, pgb.btcDupChecks, mutilchain.TYPEBTC)
	if err != nil && err != sql.ErrNoRows {
		log.Error("InsertTxns:", err)
		txRes.err = err
		return txRes
	}
	// Store tx Db IDs as funding tx in AddressRows and rearrange
	dbAddressRowsFlat := make([]*dbtypes.MutilchainAddressRow, 0, totalAddressRows)
	for it, txDbID := range *TxDbIDs {
		// Set the tx ID of the funding transactions
		for iv := range dbAddressRows[it] {
			// Transaction that pays to the address
			dba := &dbAddressRows[it][iv]
			dba.FundingTxDbID = txDbID
			// Funding tx hash, vout id, value, and address are already assigned
			// by InsertVouts. Only the funding tx DB ID was needed.
			dbAddressRowsFlat = append(dbAddressRowsFlat, dba)
		}
	}
	// Insert each new AddressRow, absent spending fields
	_, err = InsertMutilchainAddressOuts(pgb.db, dbAddressRowsFlat, mutilchain.TYPEBTC, pgb.btcDupChecks)
	if err != nil {
		log.Error("BTC: InsertAddressOuts:", err)
		txRes.err = err
		return txRes
	}
	if !updateAddressesSpendingInfo {
		return txRes
	}
	// Check the new vins and update sending tx data in Addresses table
	for it, txDbID := range *TxDbIDs {
		for iv := range dbTxVins[it] {
			// Transaction that spends an outpoint paying to >=0 addresses
			vin := &dbTxVins[it][iv]
			vinDbID := dbTransactions[it].VinDbIds[iv]
			// skip coinbase inputs
			if bytes.Equal(zeroHashStringBytes, []byte(vin.PrevTxHash)) {
				continue
			}
			var numAddressRowsSet int64
			numAddressRowsSet, err = SetMutilchainSpendingForFundingOP(pgb.db,
				vin.PrevTxHash, vin.PrevTxIndex, // funding
				txDbID, vin.TxID, vin.TxIndex, vinDbID, mutilchain.TYPEBTC) // spending
			if err != nil {
				log.Errorf("BTC: SetSpendingForFundingOP: %v", err)
			}
			txRes.numAddresses += numAddressRowsSet
		}
	}

	return txRes
}

func (pgb *ChainDB) GetMutilchainDupcheck(chainType string) bool {
	switch chainType {
	case mutilchain.TYPEBTC:
		return pgb.btcDupChecks
	case mutilchain.TYPELTC:
		return pgb.ltcDupChecks
	case mutilchain.TYPEDERO:
		return pgb.deroDupChecks
	default:
		return false
	}
}

func (pgb *ChainDB) storeMutilchainTxns(client jsonrpc.RPCClient, block *dbtypes.Block, msgBlock *structure.MsgBlock, chainType string, TxDbIDs *[]uint64,
	updateAddressesSpendingInfo bool) storeTxnsResult {
	dbTransactions, dbTxVouts, dbTxVins := dbtypes.ExtractMutilchainBlockTransactions(client, block, msgBlock, chainType)
	var txRes storeTxnsResult
	dbAddressRows := make([][]dbtypes.MutilchainAddressRow, len(dbTransactions))
	var totalAddressRows int
	var err error
	dupCheck := pgb.GetMutilchainDupcheck(chainType)
	for it, dbtx := range dbTransactions {
		dbtx.VoutDbIds, dbAddressRows[it], err = InsertMutilchainVouts(pgb.db, dbTxVouts[it], dupCheck, chainType)
		if err != nil && err != sql.ErrNoRows {
			log.Error("%s: InsertVouts: %v", chainType, err)
			txRes.err = err
			return txRes
		}
		totalAddressRows += len(dbAddressRows[it])
		txRes.numVouts += int64(len(dbtx.VoutDbIds))
		if err == sql.ErrNoRows || len(dbTxVouts[it]) != len(dbtx.VoutDbIds) {
			log.Warnf("%s: Incomplete Vout insert.", chainType)
		}
		dbtx.VinDbIds, err = InsertMutilchainVins(pgb.db, dbTxVins[it], chainType, dupCheck)
		if err != nil && err != sql.ErrNoRows {
			log.Error("%s: InsertVins: %v", chainType, err)
			txRes.err = err
			return txRes
		}
		txRes.numVins += int64(len(dbtx.VinDbIds))
	}
	// Get the tx PK IDs for storage in the blocks table
	*TxDbIDs, err = InsertMutilchainTxns(pgb.db, dbTransactions, dupCheck, chainType)
	if err != nil && err != sql.ErrNoRows {
		log.Error("%s : InsertTxns: %v", chainType, err)
		txRes.err = err
		return txRes
	}
	// Store tx Db IDs as funding tx in AddressRows and rearrange
	dbAddressRowsFlat := make([]*dbtypes.MutilchainAddressRow, 0, totalAddressRows)
	for it, txDbID := range *TxDbIDs {
		// Set the tx ID of the funding transactions
		for iv := range dbAddressRows[it] {
			// Transaction that pays to the address
			dba := &dbAddressRows[it][iv]
			dba.FundingTxDbID = txDbID
			// Funding tx hash, vout id, value, and address are already assigned
			// by InsertVouts. Only the funding tx DB ID was needed.
			dbAddressRowsFlat = append(dbAddressRowsFlat, dba)
		}
	}
	// Insert each new AddressRow, absent spending fields
	_, err = InsertMutilchainAddressOuts(pgb.db, dbAddressRowsFlat, chainType, dupCheck)
	if err != nil {
		log.Error("%s: InsertAddressOuts:%v", chainType, err)
		txRes.err = err
		return txRes
	}
	if !updateAddressesSpendingInfo {
		return txRes
	}
	// Check the new vins and update sending tx data in Addresses table
	for it, txDbID := range *TxDbIDs {
		for iv := range dbTxVins[it] {
			// Transaction that spends an outpoint paying to >=0 addresses
			vin := &dbTxVins[it][iv]
			vinDbID := dbTransactions[it].VinDbIds[iv]
			// skip coinbase inputs
			if bytes.Equal(zeroHashStringBytes, []byte(vin.PrevTxHash)) {
				continue
			}
			var numAddressRowsSet int64
			numAddressRowsSet, err = SetMutilchainSpendingForFundingOP(pgb.db,
				vin.PrevTxHash, vin.PrevTxIndex, // funding
				txDbID, vin.TxID, vin.TxIndex, vinDbID, chainType) // spending
			if err != nil {
				log.Errorf("BTC: SetSpendingForFundingOP: %v", err)
			}
			txRes.numAddresses += numAddressRowsSet
		}
	}

	return txRes
}
