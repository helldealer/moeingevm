package ebp

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
	"github.com/seehuhn/mt19937"
	dt "github.com/smartbch/moeingads/datatree"
	"github.com/smartbch/moeingads/store/rabbit"
	storetypes "github.com/smartbch/moeingads/store/types"
	modbtypes "github.com/smartbch/moeingdb/types"
	"github.com/tendermint/tendermint/libs/log"

	"github.com/smartbch/moeingevm/types"
	"github.com/smartbch/moeingevm/utils"
)

const DefaultTxGasLimit uint64 = 1000_0000

var Sep206Address = common.HexToAddress("0x0000000000000000000000000000000000002711")
var _ TxExecutor = (*txEngine)(nil)

type TxRange struct {
	start uint64
	end   uint64
}

type txEngine struct {
	// How many parallel execution round are performed for each block
	roundNum int //consensus parameter
	// How many runners execute transactions in parallel for each round
	runnerNumber int //consensus parameter
	// The runners are driven by 'parallelNum' of goroutines
	parallelNum int //per-node parameter
	// A clean Context whose RabbitStore has no cache. It must be set before calling 'Execute', and
	// txEngine will close it at the end of 'Prepare'
	cleanCtx *types.Context
	// CollectTx fills txList and 'Prepare' handles and clears txList
	txList       []*gethtypes.Transaction
	committedTxs []*types.Transaction
	// Used to check signatures
	signer       gethtypes.Signer
	currentBlock *types.BlockInfo

	cumulativeGasUsed   uint64
	cumulativeFeeRefund *uint256.Int
	cumulativeGasFee    *uint256.Int

	logger log.Logger
}

func (exec *txEngine) Context() *types.Context {
	return exec.cleanCtx
}

// Generated by parallelReadAccounts and insertToStandbyTxQ will store its tx into world state.
type preparedInfo struct {
	tx       *types.TxToRun
	txBytes  []byte
	errorStr string
}

// Generated by parallelReadAccounts and Prepare will use them for some validations.
type ctxAndAccounts struct {
	ctx          *types.Context
	accounts     []common.Address
	changed      bool                            //this ctx is changed so it must be written back
	totalGasFee  *uint256.Int                    //the gas fees payed by the accounts
	addr2nonce   map[common.Address]uint64       //caches the latest nonce of accounts and won't write back to states
	addr2Balance map[common.Address]*uint256.Int //caches latest balance
}

type frontier struct {
	addr2nonce   map[common.Address]uint64
	addr2Balance map[common.Address]*uint256.Int //caches latest balance
	addr2Gas     map[common.Address]uint64 //cache total txGas during one block
}

func NewFrontierWithCtxAA(ctxAA []*ctxAndAccounts, addr2idx map[common.Address]int) *frontier {
	nm := frontier{
		addr2nonce:   make(map[common.Address]uint64),
		addr2Balance: make(map[common.Address]*uint256.Int),
		addr2Gas:     make(map[common.Address]uint64),
	}
	for addr, idx := range addr2idx {
		if nonce, ok := ctxAA[idx].addr2nonce[addr]; ok {
			nm.addr2nonce[addr] = nonce
			nm.addr2Balance[addr] = ctxAA[idx].addr2Balance[addr]
			//there must both have nonce and balance same time
			if nm.addr2Balance[addr] == nil {
				nm.addr2Balance[addr] = uint256.NewInt(0)
			}
		}
	}
	return &nm
}

func (nm *frontier) GetLatestNonce(addr common.Address) (nonce uint64, exist bool) {
	if nm == nil {
		return 0, false
	}
	nonce, exist = nm.addr2nonce[addr]
	return nonce, exist
}

func (nm *frontier) SetLatestNonce(addr common.Address, newNonce uint64) {
	if nm == nil {
		return
	}
	nm.addr2nonce[addr] = newNonce
}

func (nm *frontier) GetLatestBalance(addr common.Address) (balance *uint256.Int, exist bool) {
	if nm == nil {
		return nil, false
	}
	balance, exist = nm.addr2Balance[addr]
	return balance, exist
}

func (nm *frontier) SetLatestBalance(addr common.Address, balance *uint256.Int) {
	if nm == nil {
		return
	}
	nm.addr2Balance[addr] = balance
}

func (nm *frontier) GetLatestTotalGas(addr common.Address) (gasLimit uint64, exist bool) {
	if nm == nil {
		return 0, false
	}
	gasLimit, exist = nm.addr2Gas[addr]
	return gasLimit, exist
}

func (nm *frontier) SetLatestTotalGas(addr common.Address, gasLimit uint64) {
	if nm == nil {
		return
	}
	nm.addr2Gas[addr] = gasLimit
}

func GetEmptyFrontier() Frontier {
	return &frontier{
		addr2nonce: make(map[common.Address]uint64),
		addr2Balance: make(map[common.Address]*uint256.Int),
		addr2Gas: make(map[common.Address]uint64),
	}
}

func NewEbpTxExec(exeRoundCount, runnerNumber, parallelNum, defaultTxListCap int, s gethtypes.Signer, logger log.Logger) *txEngine {
	Runners = make([]*TxRunner, runnerNumber)
	return &txEngine{
		roundNum:     exeRoundCount,
		runnerNumber: runnerNumber,
		parallelNum:  parallelNum,
		txList:       make([]*gethtypes.Transaction, 0, defaultTxListCap),
		committedTxs: make([]*types.Transaction, 0, defaultTxListCap),
		signer:       s,
		logger:       logger,
	}
}

// A new context must be set before Execute
func (exec *txEngine) SetContext(ctx *types.Context) {
	exec.cleanCtx = ctx
}

// Check transactions' signatures and insert the valid ones into standby queue
func (exec *txEngine) Prepare(reorderSeed int64, minGasPrice, maxTxGasLimit uint64) Frontier {
	exec.cleanCtx.Rbt.GetBaseStore().PrepareForUpdate(types.StandbyTxQueueKey[:])
	if len(exec.txList) == 0 {
		exec.cleanCtx.Close(false)
		return GetEmptyFrontier()
	}
	infoList, ctxAA := exec.parallelReadAccounts(minGasPrice, maxTxGasLimit)
	addr2idx := make(map[common.Address]int, len(exec.txList)) // map address to ctxAA's index
	for idx, entry := range ctxAA {
		for _, addr := range entry.accounts {
			if _, ok := addr2idx[addr]; !ok {
				//when we meet an address for the first time
				addr2idx[addr] = idx
			}
		}
	}
	reorderedList, addr2Infos := reorderInfoList(infoList, reorderSeed)
	ctx := exec.cleanCtx.WithRbtCopy()
	startEndBz := ctx.Rbt.GetBaseStore().Get(types.StandbyTxQueueKey[:])
	queueEnd := uint64(0)
	if len(startEndBz) >= 8 {
		queueEnd = binary.BigEndian.Uint64(startEndBz[8:])
	} else {
		startEndBz = make([]byte, 16)
	}
	ctx.Close(false)
	warmUpLen := len(reorderedList)/exec.parallelNum + 1
	dt.ParallelRun(exec.parallelNum, func(idx int) {
		entry := ctxAA[idx]
		for i := idx * warmUpLen; i < (idx+1)*warmUpLen && i < len(reorderedList); i++ {
			k := types.GetStandbyTxKey(queueEnd + uint64(i)) // warm up the entry in standby queue
			entry.ctx.Rbt.GetBaseStore().PrepareForUpdate(k)
		}
		for _, addr := range entry.accounts {
			if addr2idx[addr] != idx {
				// this addr does not belong to this entry
				continue
			}
			for _, info := range addr2Infos[addr] {
				if len(info.errorStr) != 0 {
					continue //skip it if already found error
				}
				sender := info.tx.From
				if entry.addr2nonce[sender] != info.tx.Nonce {
					//skip it if nonce is wrong
					exec.logger.Debug("prepare::incorrect nonce", "txHash", info.tx.HashID.String())
					info.errorStr = "incorrect nonce"
					continue
				}
				entry.addr2nonce[sender]++
				if exec.deductGasFeeAndUpdateFrontier(sender, info, entry) != nil {
					continue
				}
				entry.changed = true //now this context needs writeback
				info.txBytes = info.tx.ToBytes()
			}
		}
	})
	for i := range ctxAA {
		ctxAA[i].ctx.Close(ctxAA[i].changed)
	}
	// the value of exec.parallelNum and the speeds of goroutines must have
	// no effects on the order of TXs in standby queue.
	ctx = exec.cleanCtx.WithRbtCopy()
	totalGasFee := uint256.NewInt(0)
	for i := range ctxAA {
		totalGasFee.Add(totalGasFee, ctxAA[i].totalGasFee)
	}
	_ = AddSystemAccBalance(ctx, totalGasFee)
	trunk := ctx.Rbt.GetBaseStore()
	ctx.Close(true)
	exec.insertToStandbyTxQ(trunk, reorderedList, startEndBz, queueEnd)
	exec.txList = exec.txList[:0] // clear txList after consumption
	//write ctx state to trunk
	exec.cleanCtx.Close(false)
	return NewFrontierWithCtxAA(ctxAA, addr2idx)
}

func (exec *txEngine) deductGasFeeAndUpdateFrontier(sender common.Address, info *preparedInfo, entry *ctxAndAccounts) error {
	gasFee := uint256.NewInt(0).SetUint64(info.tx.Gas)
	gasFee.Mul(gasFee, utils.U256FromSlice32(info.tx.GasPrice[:]))
	err := SubSenderAccBalance(entry.ctx, sender, gasFee)
	if err != nil {
		exec.logger.Debug("prepare::deduct gas fee failed", "txHash", info.tx.HashID.String())
		entry.addr2Balance[sender] = uint256.NewInt(0)
		info.errorStr = "not enough balance to pay gasfee"
		return err
	} else {
		if info.tx.To == Sep206Address {
			entry.addr2Balance[sender] = uint256.NewInt(0)
		} else {
			if balance, exist := entry.addr2Balance[sender]; !exist {
				entry.addr2Balance[sender] = GetBalanceAfterBchTransfer(entry.ctx, sender, info.tx.Value)
			} else {
				if balance.Cmp(gasFee) < 0 {
					entry.addr2Balance[sender] = uint256.NewInt(0)
				} else {
					balance.Sub(balance, gasFee)
					txValue, _ := uint256.FromBig(utils.BigIntFromSlice32(info.tx.Value[:]))
					if balance.Cmp(txValue) < 0 {
						entry.addr2Balance[sender] = uint256.NewInt(0)
					} else {
						balance.Sub(balance, txValue)
						entry.addr2Balance[sender] = balance
					}
				}
			}
		}
		entry.totalGasFee.Add(entry.totalGasFee, gasFee)
	}
	return nil
}

func (exec *txEngine) getCurrHeight() uint64 {
	if exec.currentBlock != nil {
		return uint64(exec.currentBlock.Number)
	}
	return 0
}

// Read accounts' information in parallel, while checking accounts' existence and signatures' validity
func (exec *txEngine) parallelReadAccounts(minGasPrice, maxTxGasLimit uint64) (infoList []*preparedInfo, ctxAA []*ctxAndAccounts) {
	//for each tx, we fetch some info for it
	infoList = make([]*preparedInfo, len(exec.txList))
	//the ctx and accounts that a worker works at
	ctxAA = make([]*ctxAndAccounts, exec.parallelNum)
	sharedIdx := int64(-1)
	estimatedSize := len(exec.txList)/exec.parallelNum + 1
	dt.ParallelRun(exec.parallelNum, func(workerId int) {
		ctxAA[workerId] = &ctxAndAccounts{
			ctx:          exec.cleanCtx.WithRbtCopy(),
			accounts:     make([]common.Address, 0, estimatedSize),
			changed:      false,
			totalGasFee:  uint256.NewInt(0),
			addr2nonce:   make(map[common.Address]uint64, estimatedSize),
			addr2Balance: make(map[common.Address]*uint256.Int, estimatedSize),
		}
		for {
			myIdx := atomic.AddInt64(&sharedIdx, 1)
			if int(myIdx) >= len(exec.txList) {
				return
			}
			tx := exec.txList[myIdx]
			infoList[myIdx] = &preparedInfo{}
			// we need some computation to get the sender's address
			sender, err := exec.signer.Sender(tx)
			//set txToRun first
			txToRun := &types.TxToRun{}
			txToRun.FromGethTx(tx, sender, exec.getCurrHeight())
			infoList[myIdx].tx = txToRun
			if err != nil {
				infoList[myIdx].errorStr = "invalid signature"
				continue
			}
			if !tx.GasPrice().IsInt64() || tx.GasPrice().Int64() < int64(minGasPrice) {
				infoList[myIdx].errorStr = "invalid gas price"
				continue
			}
			if tx.Gas() > maxTxGasLimit {
				infoList[myIdx].errorStr = "invalid gas limit"
				continue
			}
			// access disk to fetch the account's detail
			acc := ctxAA[workerId].ctx.GetAccount(sender)
			if acc == nil {
				infoList[myIdx].errorStr = "non-existent account"
				continue
			}
			if _, ok := ctxAA[workerId].addr2nonce[sender]; !ok {
				ctxAA[workerId].accounts = append(ctxAA[workerId].accounts, sender)
				ctxAA[workerId].addr2nonce[sender] = acc.Nonce()
				ctxAA[workerId].addr2Balance[sender] = acc.Balance().Clone()
			}
		}
	})
	return
}

func reorderInfoList(infoList []*preparedInfo, reorderSeed int64) (out []*preparedInfo, addr2Infos map[common.Address][]*preparedInfo) {
	out = make([]*preparedInfo, 0, len(infoList))
	addr2Infos = make(map[common.Address][]*preparedInfo, len(infoList))
	addrList := make([]common.Address, 0, len(infoList))
	for _, info := range infoList {
		if _, ok := addr2Infos[info.tx.From]; ok {
			addr2Infos[info.tx.From] = append(addr2Infos[info.tx.From], info)
		} else {
			addr2Infos[info.tx.From] = []*preparedInfo{info}
			addrList = append(addrList, info.tx.From)
		}
	}
	rand := mt19937.New()
	rand.Seed(reorderSeed)
	for i := 0; i < len(addrList); i++ { // shuffle the addresses
		r0 := int(rand.Int63()) % len(addrList)
		r1 := int(rand.Int63()) % len(addrList)
		addrList[r0], addrList[r1] = addrList[r1], addrList[r0]
	}
	for _, addr := range addrList {
		out = append(out, addr2Infos[addr]...)
	}
	return
}

// insert valid transactions into standby queue
func (exec *txEngine) insertToStandbyTxQ(trunk storetypes.BaseStoreI, infoList []*preparedInfo, startEnd []byte, end uint64) {
	trunk.Update(func(store storetypes.SetDeleter) {
		for _, info := range infoList {
			// six kinds of errors: invalid signature; incorrect nonce;
			// no such account; balance not enough; gas limit too high; gas price too low;
			// if the proposor is honest, there should be no these kinds of errors.
			if len(info.errorStr) != 0 {
				exec.recordInvalidTx(info)
				continue
			}
			k := types.GetStandbyTxKey(end)
			store.Set(k, info.txBytes)
			end++
		}
		binary.BigEndian.PutUint64(startEnd[8:], end)
		store.Set(types.StandbyTxQueueKey[:], startEnd) //update start&end pointers of standby queue
	})
}

func (exec *txEngine) recordInvalidTx(info *preparedInfo) {
	tx := &types.Transaction{
		Hash:              info.tx.HashID,
		TransactionIndex:  int64(len(exec.committedTxs)),
		Nonce:             info.tx.Nonce,
		BlockNumber:       int64(exec.getCurrHeight()),
		From:              info.tx.From,
		To:                info.tx.To,
		Value:             info.tx.Value,
		GasPrice:          info.tx.GasPrice,
		Gas:               info.tx.Gas,
		Input:             info.tx.Data,
		CumulativeGasUsed: exec.cumulativeGasUsed,
		GasUsed:           0,
		Status:            gethtypes.ReceiptStatusFailed,
		StatusStr:         info.errorStr,
	}
	if exec.currentBlock != nil {
		tx.BlockHash = exec.currentBlock.Hash
	}
	exec.committedTxs = append(exec.committedTxs, tx)
}

// Fetch TXs from standby queue and execute them
func (exec *txEngine) Execute(currBlock *types.BlockInfo) {
	exec.committedTxs = exec.committedTxs[:0]
	exec.cumulativeGasUsed = 0
	exec.cumulativeFeeRefund = uint256.NewInt(0)
	exec.cumulativeGasFee = uint256.NewInt(0)
	exec.currentBlock = currBlock
	startKey, endKey := exec.getStandbyQueueRange()
	if startKey == endKey {
		return
	}
	txRange := &TxRange{
		start: startKey,
		end:   endKey,
	}
	committableRunnerList := make([]*TxRunner, 0, 4096)
	// Repeat exec.roundNum round for execute txs in standby q. At the end of each round
	// modifications made by TXs are written to world state. So TXs in later rounds can
	// see the modifications made by TXs in earlier rounds.
	for i := 0; i < exec.roundNum; i++ {
		if txRange.start == txRange.end {
			break
		}
		numTx := exec.executeOneRound(txRange, exec.currentBlock)
		for i := 0; i < numTx; i++ {
			if Runners[i] == nil {
				continue // the TX is not committable and needs re-execution
			}
			committableRunnerList = append(committableRunnerList, Runners[i])
			Runners[i] = nil
		}
	}
	exec.setStandbyQueueRange(txRange.start, txRange.end)
	exec.collectCommittableTxs(committableRunnerList)
}

// Get the start and end position of standby queue
func (exec *txEngine) getStandbyQueueRange() (start, end uint64) {
	ctx := exec.cleanCtx.WithRbtCopy()
	defer ctx.Close(false)
	startEnd := ctx.Rbt.GetBaseStore().Get(types.StandbyTxQueueKey[:])
	if startEnd == nil {
		return 0, 0
	}
	return binary.BigEndian.Uint64(startEnd[:8]), binary.BigEndian.Uint64(startEnd[8:])
}

// Set the start and end position of standby queue
func (exec *txEngine) setStandbyQueueRange(start, end uint64) {
	startEnd := make([]byte, 16)
	binary.BigEndian.PutUint64(startEnd[:8], start)
	binary.BigEndian.PutUint64(startEnd[8:], end)
	trunk := exec.cleanCtx.Rbt.GetBaseStore()
	trunk.Update(func(store storetypes.SetDeleter) {
		store.Set(types.StandbyTxQueueKey[:], startEnd)
	})
}

// Execute 'runnerNumber' transactions in parallel and commit the ones without any interdependency
func (exec *txEngine) executeOneRound(txRange *TxRange, currBlock *types.BlockInfo) int {
	txBundle := exec.loadStandbyTxs(txRange)
	kvCount := exec.runTxInParallel(txRange, txBundle, currBlock)
	exec.checkTxDepsAndUptStandbyQ(txRange, txBundle, int(kvCount))
	return len(txBundle)
}

// Load at most 'exec.runnerNumber' transactions from standby queue
func (exec *txEngine) loadStandbyTxs(txRange *TxRange) (txBundle []types.TxToRun) {
	ctx := exec.cleanCtx.WithRbtCopy()
	end := txRange.end
	if end > txRange.start+uint64(exec.runnerNumber) { // load at most exec.runnerNumber
		end = txRange.start + uint64(exec.runnerNumber)
	}
	txBundle = make([]types.TxToRun, end-txRange.start)
	for i := txRange.start; i < end; i++ {
		k := types.GetStandbyTxKey(i)
		bz := ctx.Rbt.GetBaseStore().Get(k)
		txBundle[i-txRange.start].FromBytes(bz)
	}
	ctx.Close(false)
	return
}

// Assign the transactions to global 'Runners' and run them in parallel.
// Record the count of touched KV pairs and return it as a hint for checkTxDepsAndUptStandbyQ
func (exec *txEngine) runTxInParallel(txRange *TxRange, txBundle []types.TxToRun, currBlock *types.BlockInfo) (kvCount int64) {
	sharedIdx := int64(-1)
	dt.ParallelRun(exec.parallelNum, func(_ int) {
		for {
			myIdx := atomic.AddInt64(&sharedIdx, 1)
			if myIdx >= int64(len(txBundle)) {
				return
			}
			Runners[myIdx] = &TxRunner{
				Ctx: exec.cleanCtx.WithRbtCopy(),
				Tx:  &txBundle[myIdx],
			}
			k := types.GetStandbyTxKey(txRange.start + uint64(myIdx))
			Runners[myIdx].Ctx.Rbt.GetBaseStore().PrepareForDeletion(k) // remove it from the standby queue
			k = types.GetStandbyTxKey(txRange.end + uint64(myIdx))
			Runners[myIdx].Ctx.Rbt.GetBaseStore().PrepareForUpdate(k) //warm up
			if myIdx > 0 && txBundle[myIdx-1].From == txBundle[myIdx].From {
				// In reorderInfoList, we placed the tx with same 'From' back-to-back
				// same from-address as previous transaction, cannot run in same round
				Runners[myIdx].Status = types.TX_NONCE_TOO_LARGE
			} else {
				runTx(int(myIdx), currBlock)
				atomic.AddInt64(&kvCount, int64(Runners[myIdx].Ctx.Rbt.CachedEntryCount()))
			}
		}
	})
	return
}

type indexAndBool struct {
	idx       int
	canCommit bool
}

// Check interdependency of TXs using 'touchedSet'. The ones with dependency with former committed TXs cannot
// be committed and should be inserted back into the standby queue.
func (exec *txEngine) checkTxDepsAndUptStandbyQ(txRange *TxRange, txBundle []types.TxToRun, kvCount int) {
	touchedSet := make(map[uint64]struct{}, kvCount)
	var wg sync.WaitGroup
	idxChan := make(chan indexAndBool, 10)
	wg.Add(1)
	go func() { // this goroutine commits the rabbit stores
		for {
			idxAndBool := <-idxChan
			if idxAndBool.idx < 0 {
				break
			}
			Runners[idxAndBool.idx].Ctx.Rbt.CloseAndWriteBack(idxAndBool.canCommit)
		}
		wg.Done()
	}()
	for idx := range txBundle {
		canCommit := true
		Runners[idx].Ctx.Rbt.ScanAllShortKeys(func(key [rabbit.KeySize]byte, dirty bool) (stop bool) {
			k := binary.LittleEndian.Uint64(key[:])
			if _, ok := touchedSet[k]; ok {
				canCommit = false // cannot commit if conflicts with touched KV set
				Runners[idx].Status = types.FAILED_TO_COMMIT
				return true
			} else {
				return false
			}
		})
		if canCommit { // record the dirty KVs written by a committable TX into toucchedSet
			Runners[idx].Ctx.Rbt.ScanAllShortKeys(func(key [rabbit.KeySize]byte, dirty bool) (stop bool) {
				if dirty {
					k := binary.LittleEndian.Uint64(key[:])
					touchedSet[k] = struct{}{}
				}
				return false
			})
		}
		idxChan <- indexAndBool{idx, canCommit}
	}
	idxChan <- indexAndBool{-1, false}
	wg.Wait()

	trunk := exec.cleanCtx.Rbt.GetBaseStore()
	trunk.Update(func(store storetypes.SetDeleter) {
		for idx, tx := range txBundle {
			status := Runners[idx].Status
			k := types.GetStandbyTxKey(txRange.start)
			txRange.start++
			store.Delete(k)
			if status == types.FAILED_TO_COMMIT || status == types.TX_NONCE_TOO_LARGE {
				newK := types.GetStandbyTxKey(txRange.end)
				txRange.end++
				store.Set(newK, tx.ToBytes()) // insert the failed TXs back into standby queue
				Runners[idx] = nil
			} else if status == types.ACCOUNT_NOT_EXIST || status == types.TX_NONCE_TOO_SMALL {
				//collect invalid tx`s all gas
				exec.cumulativeGasUsed += Runners[idx].Tx.Gas
				exec.cumulativeGasFee.Add(exec.cumulativeGasFee, Runners[idx].GetGasFee())
				Runners[idx] = nil
			}
		}
	})
}

// Fill 'exec.committedTxs' with 'committableRunnerList'
func (exec *txEngine) collectCommittableTxs(committableRunnerList []*TxRunner) {
	var logIndex uint
	for idx, runner := range committableRunnerList {
		exec.cumulativeGasUsed += runner.GasUsed
		exec.cumulativeFeeRefund.Add(exec.cumulativeFeeRefund, &runner.FeeRefund)
		exec.cumulativeGasFee.Add(exec.cumulativeGasFee, runner.GetGasFee())
		tx := &types.Transaction{
			Hash:              runner.Tx.HashID,
			TransactionIndex:  int64(idx),
			Nonce:             runner.Tx.Nonce,
			BlockHash:         exec.currentBlock.Hash,
			BlockNumber:       exec.currentBlock.Number,
			From:              runner.Tx.From,
			To:                runner.Tx.To,
			Value:             runner.Tx.Value,
			GasPrice:          runner.Tx.GasPrice,
			Gas:               runner.Tx.Gas,
			Input:             runner.Tx.Data,
			CumulativeGasUsed: exec.cumulativeGasUsed,
			GasUsed:           runner.GasUsed,
			ContractAddress:   runner.CreatedContractAddress, //20 Bytes - the contract address created, if the transaction was a contract creation, otherwise - null.
			OutData:           append([]byte{}, runner.OutData...),
			Status:            gethtypes.ReceiptStatusSuccessful,
			StatusStr:         StatusToStr(runner.Status),
			InternalTxCalls:   runner.InternalTxCalls,
			InternalTxReturns: runner.InternalTxReturns,
		}
		exec.logger.Debug("collectCommittableTxs:", "status", tx.StatusStr, "hash", common.Hash(tx.Hash).String())
		if StatusIsFailure(runner.Status) {
			tx.Status = gethtypes.ReceiptStatusFailed
		}
		tx.Logs = make([]types.Log, len(runner.Logs))
		for i, log := range runner.Logs {
			copy(tx.Logs[i].Address[:], log.Address[:])
			tx.Logs[i].Topics = make([][32]byte, len(log.Topics))
			for j, t := range log.Topics {
				copy(tx.Logs[i].Topics[j][:], t[:])
			}
			tx.Logs[i].Data = log.Data
			log.Data = nil
			tx.Logs[i].BlockNumber = uint64(exec.currentBlock.Number)
			copy(tx.Logs[i].BlockHash[:], exec.currentBlock.Hash[:])
			copy(tx.Logs[i].TxHash[:], tx.Hash[:])
			//txIndex = index in committableRunnerList
			tx.Logs[i].TxIndex = uint(idx)
			tx.Logs[i].Index = logIndex
			logIndex++
			tx.Logs[i].Removed = false
		}
		tx.LogsBloom = LogsBloom(tx.Logs)
		exec.committedTxs = append(exec.committedTxs, tx)
	}
}

func LogsBloom(logs []types.Log) [256]byte {
	var bin gethtypes.Bloom
	for _, log := range logs {
		bin.Add(log.Address[:])
		for _, b := range log.Topics {
			bin.Add(b[:])
		}
	}
	return bin
}

func (exec *txEngine) CommittedTxs() []*types.Transaction {
	return exec.committedTxs
}

func (exec *txEngine) CommittedTxIds() [][32]byte {
	idList := make([][32]byte, len(exec.committedTxs))
	for i, tx := range exec.committedTxs {
		idList[i] = tx.Hash
	}
	return idList
}

func (exec *txEngine) CommittedTxsForMoDB() []modbtypes.Tx {
	txList := make([]modbtypes.Tx, len(exec.committedTxs))
	for i, tx := range exec.committedTxs {
		t := modbtypes.Tx{}
		copy(t.HashId[:], tx.Hash[:])
		copy(t.SrcAddr[:], tx.From[:])
		copy(t.DstAddr[:], tx.To[:])
		txContent, err := tx.MarshalMsg(nil)
		if err != nil {
			panic(err)
		}
		t.Content = txContent
		t.LogList = make([]modbtypes.Log, len(tx.Logs))
		for j, l := range tx.Logs {
			copy(t.LogList[j].Address[:], l.Address[:])
			if len(l.Topics) != 0 {
				t.LogList[j].Topics = make([][32]byte, len(l.Topics))
			}
			for k, topic := range l.Topics {
				copy(t.LogList[j].Topics[k][:], topic[:])
			}
		}
		txList[i] = t
	}
	return txList
}

func (exec *txEngine) CollectTx(tx *gethtypes.Transaction) {
	exec.txList = append(exec.txList, tx)
}

func (exec *txEngine) CollectedTxsCount() int {
	return len(exec.txList)
}

func (exec *txEngine) GasUsedInfo() (gasUsed uint64, feeRefund, gasFee uint256.Int) {
	if exec.cumulativeGasFee == nil {
		return exec.cumulativeGasUsed, *exec.cumulativeFeeRefund, uint256.Int{}
	}
	return exec.cumulativeGasUsed, *exec.cumulativeFeeRefund, *exec.cumulativeGasFee
}

func (exec *txEngine) StandbyQLen() int {
	s, e := exec.getStandbyQueueRange()
	return int(e - s)
}

var (
	// record pending gas fee and refund
	systemContractAddress = [20]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		byte('s'), byte('y'), byte('s'), byte('t'), byte('e'), byte('m')}
	// record distribute fee
	blackHoleContractAddress = [20]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		byte('b'), byte('l'), byte('a'), byte('c'), byte('k'), byte('h'), byte('o'), byte('l'), byte('e')}
)

// guarantee account exists externally
func SubSenderAccBalance(ctx *types.Context, sender common.Address, amount *uint256.Int) error {
	return updateBalance(ctx, sender, amount, false)
}

func AddSystemAccBalance(ctx *types.Context, amount *uint256.Int) error {
	return updateBalance(ctx, systemContractAddress, amount, true)
}

func SubSystemAccBalance(ctx *types.Context, amount *uint256.Int) error {
	return updateBalance(ctx, systemContractAddress, amount, false)
}

func TransferFromSenderAccToBlackHoleAcc(ctx *types.Context, sender common.Address, amount *uint256.Int) error {
	err := updateBalance(ctx, sender, amount, false)
	if err != nil {
		return err
	}
	return updateBalance(ctx, blackHoleContractAddress, amount, true)
}

// will lazy init account if acc not exist
func updateBalance(ctx *types.Context, address common.Address, amount *uint256.Int, isAdd bool) error {
	acc := ctx.GetAccount(address)
	if acc == nil {
		//lazy init
		acc = types.ZeroAccountInfo()
	}
	s := acc.Balance()
	if isAdd {
		acc.UpdateBalance(s.Add(s, amount))
	} else {
		if s.Cmp(amount) < 0 {
			return errors.New("balance not enough")
		}
		acc.UpdateBalance(s.Sub(s, amount))
	}
	ctx.SetAccount(address, acc)
	return nil
}

func GetBalanceAfterBchTransfer(ctx *types.Context, address common.Address, value [32]byte) *uint256.Int {
	acc := ctx.GetAccount(address)
	if acc == nil {
		return uint256.NewInt(0)
	}
	out := acc.Balance().Clone()
	txValue, _ := uint256.FromBig(utils.BigIntFromSlice32(value[:]))
	if out.Cmp(txValue) < 0 {
		return uint256.NewInt(0)
	}
	return out.Sub(out, txValue)
}

func GetSystemBalance(ctx *types.Context) *uint256.Int {
	acc := ctx.GetAccount(systemContractAddress)
	if acc == nil {
		acc = types.ZeroAccountInfo()
	}
	return acc.Balance()
}

func GetBlackHoleBalance(ctx *types.Context) *uint256.Int {
	acc := ctx.GetAccount(blackHoleContractAddress)
	if acc == nil {
		acc = types.ZeroAccountInfo()
	}
	return acc.Balance()
}
