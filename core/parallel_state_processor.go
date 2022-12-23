// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type ParallelStateProcessor struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

type ParallelStateProcessorProfile struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

type ParallelStateProcessorUse struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

// NewStateProcessor initialises a new StateProcessor.
func NewParallelStateProcessor(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *ParallelStateProcessor {
	return &ParallelStateProcessor{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

func NewParallelStateProcessorProfile(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *ParallelStateProcessorProfile {
	return &ParallelStateProcessorProfile{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

func NewParallelStateProcessorUse(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *ParallelStateProcessorUse {
	return &ParallelStateProcessorUse{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

type ExecutionTask struct {
	msg    types.Message
	config *params.ChainConfig

	gasLimit                   uint64
	blockNumber                *big.Int
	blockHash                  common.Hash
	tx                         *types.Transaction
	index                      int
	statedb                    *state.StateDB // State database that stores the modified values after tx execution.
	cleanStateDB               *state.StateDB // A clean copy of the initial statedb. It should not be modified.
	finalStateDB               *state.StateDB // The final statedb.
	header                     *types.Header
	blockChain                 *BlockChain
	evmConfig                  vm.Config
	result                     *ExecutionResult
	shouldDelayFeeCal          *bool
	shouldRerunWithoutFeeDelay bool
	sender                     common.Address
	totalUsedGas               *uint64
	receipts                   *types.Receipts
	allLogs                    *[]*types.Log

	// length of dependencies          -> 2 + k (k = a whole number)
	// first 2 element in dependencies -> transaction index, and flag representing if delay is allowed or not
	//                                       (0 -> delay is not allowed, 1 -> delay is allowed)
	// next k elements in dependencies -> transaction indexes on which transaction i is dependent on
	dependencies []int

	blockContext vm.BlockContext
	coinbase     common.Address
}

func (task *ExecutionTask) Execute(mvh *blockstm.MVHashMap, incarnation int) (err error) {
	task.statedb = task.cleanStateDB.Copy()
	task.statedb.Prepare(task.tx.Hash(), task.index)
	task.statedb.SetMVHashmap(mvh)
	task.statedb.SetIncarnation(incarnation)

	evm := vm.NewEVM(task.blockContext, vm.TxContext{}, task.statedb, task.config, task.evmConfig)

	// Create a new context to be used in the EVM environment.
	txContext := NewEVMTxContext(task.msg)
	evm.Reset(txContext, task.statedb)

	defer func() {
		if r := recover(); r != nil {
			// In some pre-matured executions, EVM will panic. Recover from panic and retry the execution.
			log.Debug("Recovered from EVM failure.", "Error:", r)

			err = blockstm.ErrExecAbortError{Dependency: task.statedb.DepTxIndex()}

			return
		}
	}()

	// Apply the transaction to the current state (included in the env).
	if *task.shouldDelayFeeCal {
		task.result, err = ApplyMessageNoFeeBurnOrTip(evm, task.msg, new(GasPool).AddGas(task.gasLimit))

		if task.result == nil || err != nil {
			return blockstm.ErrExecAbortError{Dependency: task.statedb.DepTxIndex(), OriginError: err}
		}

		reads := task.statedb.MVReadMap()

		if _, ok := reads[blockstm.NewSubpathKey(task.blockContext.Coinbase, state.BalancePath)]; ok {
			log.Info("Coinbase is in MVReadMap", "address", task.blockContext.Coinbase)

			task.shouldRerunWithoutFeeDelay = true
		}

		if _, ok := reads[blockstm.NewSubpathKey(task.result.BurntContractAddress, state.BalancePath)]; ok {
			log.Info("BurntContractAddress is in MVReadMap", "address", task.result.BurntContractAddress)

			task.shouldRerunWithoutFeeDelay = true
		}
	} else {
		task.result, err = ApplyMessage(evm, task.msg, new(GasPool).AddGas(task.gasLimit))
	}

	if task.statedb.HadInvalidRead() || err != nil {
		err = blockstm.ErrExecAbortError{Dependency: task.statedb.DepTxIndex(), OriginError: err}
		return
	}

	task.statedb.Finalise(task.config.IsEIP158(task.blockNumber))

	return
}

func (task *ExecutionTask) MVReadList() []blockstm.ReadDescriptor {
	return task.statedb.MVReadList()
}

func (task *ExecutionTask) MVWriteList() []blockstm.WriteDescriptor {
	return task.statedb.MVWriteList()
}

func (task *ExecutionTask) MVFullWriteList() []blockstm.WriteDescriptor {
	return task.statedb.MVFullWriteList()
}

func (task *ExecutionTask) Sender() common.Address {
	return task.sender
}

func (task *ExecutionTask) Hash() common.Hash {
	return task.tx.Hash()
}

func (task *ExecutionTask) Dependencies() []int {
	return task.dependencies
}

func (task *ExecutionTask) Settle() {
	defer func() {
		if r := recover(); r != nil {
			// In some rare cases, ApplyMVWriteSet will panic due to an index out of range error when calculating the
			// address hash in sha3 module. Recover from panic and continue the execution.
			// After recovery, block receipts or merckle root will be incorrect, but this is fine, because the block
			// will be rejected and re-synced.
			log.Info("Recovered from error", "Error:", r)
			return
		}
	}()

	task.finalStateDB.Prepare(task.tx.Hash(), task.index)

	coinbaseBalance := task.finalStateDB.GetBalance(task.coinbase)

	task.finalStateDB.ApplyMVWriteSet(task.statedb.MVWriteList())

	for _, l := range task.statedb.GetLogs(task.tx.Hash(), task.blockHash) {
		task.finalStateDB.AddLog(l)
	}

	if *task.shouldDelayFeeCal {
		if task.config.IsLondon(task.blockNumber) {
			task.finalStateDB.AddBalance(task.result.BurntContractAddress, task.result.FeeBurnt)
		}

		task.finalStateDB.AddBalance(task.coinbase, task.result.FeeTipped)
		output1 := new(big.Int).SetBytes(task.result.SenderInitBalance.Bytes())
		output2 := new(big.Int).SetBytes(coinbaseBalance.Bytes())

		// Deprecating transfer log and will be removed in future fork. PLEASE DO NOT USE this transfer log going forward. Parameters won't get updated as expected going forward with EIP1559
		// add transfer log
		AddFeeTransferLog(
			task.finalStateDB,

			task.msg.From(),
			task.coinbase,

			task.result.FeeTipped,
			task.result.SenderInitBalance,
			coinbaseBalance,
			output1.Sub(output1, task.result.FeeTipped),
			output2.Add(output2, task.result.FeeTipped),
		)
	}

	for k, v := range task.statedb.Preimages() {
		task.finalStateDB.AddPreimage(k, v)
	}

	// Update the state with pending changes.
	var root []byte

	if task.config.IsByzantium(task.blockNumber) {
		task.finalStateDB.Finalise(true)
	} else {
		root = task.finalStateDB.IntermediateRoot(task.config.IsEIP158(task.blockNumber)).Bytes()
	}

	*task.totalUsedGas += task.result.UsedGas

	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt := &types.Receipt{Type: task.tx.Type(), PostState: root, CumulativeGasUsed: *task.totalUsedGas}
	if task.result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}

	receipt.TxHash = task.tx.Hash()
	receipt.GasUsed = task.result.UsedGas

	// If the transaction created a contract, store the creation address in the receipt.
	if task.msg.To() == nil {
		receipt.ContractAddress = crypto.CreateAddress(task.msg.From(), task.tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = task.finalStateDB.GetLogs(task.tx.Hash(), task.blockHash)
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	receipt.BlockHash = task.blockHash
	receipt.BlockNumber = task.blockNumber
	receipt.TransactionIndex = uint(task.finalStateDB.TxIndex())

	*task.receipts = append(*task.receipts, receipt)
	*task.allLogs = append(*task.allLogs, receipt.Logs...)
}

var parallelizabilityTimer = metrics.NewRegisteredTimer("block/parallelizability", nil)

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
// nolint:gocognit
func (p *ParallelStateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	var (
		receipts    types.Receipts
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		usedGas     = new(uint64)
		metadata    bool
	)

	// Mutate the block and state according to any hard-fork specs
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}

	tasks := make([]blockstm.ExecTask, 0, len(block.Transactions()))

	shouldDelayFeeCal := true

	coinbase, _ := p.bc.Engine().Author(header)

	deps, delayMap := GetDeps(block.Header().TxDependency)

	if block.Header().TxDependency != nil {
		metadata = true
	}

	for _, j := range delayMap {
		if !j {
			log.Info("BlockSTM", "Dependencies deps", deps)
			log.Info("BlockSTM", "Dependencies delayMap", delayMap)
			log.Info("Going Serial", "!j", !j)
			pSeral := NewStateProcessor(p.config, p.bc, p.engine)
			return pSeral.Process(block, statedb, cfg)
		}
	}

	blockContext := NewEVMBlockContext(header, p.bc, nil)
	// p.bc.Engine().Author(header)
	for i, tx := range block.Transactions() {
		msg, err := tx.AsMessage(types.MakeSigner(p.config, header.Number), header.BaseFee)
		if err != nil {
			log.Error("error creating message", "err", err)
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}

		cleansdb := statedb.Copy()

		if len(header.TxDependency) > 0 {
			shouldDelayFeeCal = delayMap[i]

			task := &ExecutionTask{
				msg:               msg,
				config:            p.config,
				gasLimit:          block.GasLimit(),
				blockNumber:       blockNumber,
				blockHash:         blockHash,
				tx:                tx,
				index:             i,
				cleanStateDB:      cleansdb,
				finalStateDB:      statedb,
				blockChain:        p.bc,
				header:            header,
				evmConfig:         cfg,
				shouldDelayFeeCal: &shouldDelayFeeCal,
				sender:            msg.From(),
				totalUsedGas:      usedGas,
				receipts:          &receipts,
				allLogs:           &allLogs,
				dependencies:      deps[i],
				blockContext:      blockContext,
				coinbase:          coinbase,
			}

			tasks = append(tasks, task)
		} else {
			if msg.From() == coinbase {
				shouldDelayFeeCal = false
			}

			task := &ExecutionTask{
				msg:               msg,
				config:            p.config,
				gasLimit:          block.GasLimit(),
				blockNumber:       blockNumber,
				blockHash:         blockHash,
				tx:                tx,
				index:             i,
				cleanStateDB:      cleansdb,
				finalStateDB:      statedb,
				blockChain:        p.bc,
				header:            header,
				evmConfig:         cfg,
				shouldDelayFeeCal: &shouldDelayFeeCal,
				sender:            msg.From(),
				totalUsedGas:      usedGas,
				receipts:          &receipts,
				allLogs:           &allLogs,
				dependencies:      nil,
				blockContext:      blockContext,
				coinbase:          coinbase,
			}

			tasks = append(tasks, task)
		}
	}

	backupStateDB := statedb.Copy()

	profile := false
	result, err := blockstm.ExecuteParallel(tasks, false, metadata)

	if err == nil && profile {
		_, weight := result.Deps.LongestPath(*result.Stats)

		serialWeight := uint64(0)

		for i := 0; i < len(result.Deps.GetVertices()); i++ {
			serialWeight += (*result.Stats)[i].End - (*result.Stats)[i].Start
		}

		parallelizabilityTimer.Update(time.Duration(serialWeight * 100 / weight))

		log.Info("Parallelizability", "Average (%)", parallelizabilityTimer.Mean())

		log.Info("Parallelizability", "Histogram (%)", parallelizabilityTimer.Percentiles([]float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 0.999, 0.9999}))
	}

	for _, task := range tasks {
		task := task.(*ExecutionTask)
		if task.shouldRerunWithoutFeeDelay {
			shouldDelayFeeCal = false
			*statedb = *backupStateDB

			allLogs = []*types.Log{}
			receipts = types.Receipts{}
			usedGas = new(uint64)

			for _, t := range tasks {
				t := t.(*ExecutionTask)
				t.finalStateDB = backupStateDB
				t.allLogs = &allLogs
				t.receipts = &receipts
				t.totalUsedGas = usedGas
			}

			_, err = blockstm.ExecuteParallel(tasks, false, metadata)

			break
		}
	}

	if err != nil {
		log.Error("blockstm error executing block", "err", err)
		return nil, nil, 0, err
	}

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	p.engine.Finalize(p.bc, header, statedb, block.Transactions(), block.Uncles())

	return receipts, allLogs, *usedGas, nil
}

func (p *ParallelStateProcessorProfile) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	var (
		receipts    types.Receipts
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		usedGas     = new(uint64)
		metadata    bool
	)

	// Mutate the block and state according to any hard-fork specs
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}

	tasks := make([]blockstm.ExecTask, 0, len(block.Transactions()))

	shouldDelayFeeCal := true

	coinbase, _ := p.bc.Engine().Author(header)

	deps, delayMap := GetDeps(block.Header().TxDependency)

	if block.Header().TxDependency != nil {
		metadata = true
	}

	for _, j := range delayMap {
		if !j {
			log.Info("BlockSTM", "Dependencies deps", deps)
			log.Info("BlockSTM", "Dependencies delayMap", delayMap)
			log.Info("Going Serial", "!j", !j)
			pSeral := NewStateProcessor(p.config, p.bc, p.engine)
			return pSeral.Process(block, statedb, cfg)
		}
	}

	blockContext := NewEVMBlockContext(header, p.bc, nil)
	// p.bc.Engine().Author(header)
	for i, tx := range block.Transactions() {
		msg, err := tx.AsMessage(types.MakeSigner(p.config, header.Number), header.BaseFee)
		if err != nil {
			log.Error("error creating message", "err", err)
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}

		cleansdb := statedb.Copy()

		if len(header.TxDependency) > 0 {
			shouldDelayFeeCal = delayMap[i]

			task := &ExecutionTask{
				msg:               msg,
				config:            p.config,
				gasLimit:          block.GasLimit(),
				blockNumber:       blockNumber,
				blockHash:         blockHash,
				tx:                tx,
				index:             i,
				cleanStateDB:      cleansdb,
				finalStateDB:      statedb,
				blockChain:        p.bc,
				header:            header,
				evmConfig:         cfg,
				shouldDelayFeeCal: &shouldDelayFeeCal,
				sender:            msg.From(),
				totalUsedGas:      usedGas,
				receipts:          &receipts,
				allLogs:           &allLogs,
				dependencies:      deps[i],
				blockContext:      blockContext,
				coinbase:          coinbase,
			}

			tasks = append(tasks, task)
		} else {
			if msg.From() == coinbase {
				shouldDelayFeeCal = false
			}

			task := &ExecutionTask{
				msg:               msg,
				config:            p.config,
				gasLimit:          block.GasLimit(),
				blockNumber:       blockNumber,
				blockHash:         blockHash,
				tx:                tx,
				index:             i,
				cleanStateDB:      cleansdb,
				finalStateDB:      statedb,
				blockChain:        p.bc,
				header:            header,
				evmConfig:         cfg,
				shouldDelayFeeCal: &shouldDelayFeeCal,
				sender:            msg.From(),
				totalUsedGas:      usedGas,
				receipts:          &receipts,
				allLogs:           &allLogs,
				dependencies:      nil,
				blockContext:      blockContext,
				coinbase:          coinbase,
			}

			tasks = append(tasks, task)
		}
	}

	backupStateDB := statedb.Copy()

	profile := false
	result, err := blockstm.ExecuteParallel(tasks, true, metadata)

	if err == nil && profile {
		_, weight := result.Deps.LongestPath(*result.Stats)

		serialWeight := uint64(0)

		for i := 0; i < len(result.Deps.GetVertices()); i++ {
			serialWeight += (*result.Stats)[i].End - (*result.Stats)[i].Start
		}

		parallelizabilityTimer.Update(time.Duration(serialWeight * 100 / weight))

		log.Info("Parallelizability", "Average (%)", parallelizabilityTimer.Mean())

		log.Info("Parallelizability", "Histogram (%)", parallelizabilityTimer.Percentiles([]float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 0.999, 0.9999}))
	}

	for _, task := range tasks {
		task := task.(*ExecutionTask)
		if task.shouldRerunWithoutFeeDelay {
			shouldDelayFeeCal = false
			*statedb = *backupStateDB

			allLogs = []*types.Log{}
			receipts = types.Receipts{}
			usedGas = new(uint64)

			for _, t := range tasks {
				t := t.(*ExecutionTask)
				t.finalStateDB = backupStateDB
				t.allLogs = &allLogs
				t.receipts = &receipts
				t.totalUsedGas = usedGas
			}

			result, err = blockstm.ExecuteParallel(tasks, true, metadata)

			break
		}
	}

	if err != nil {
		log.Error("blockstm error executing block", "err", err)
		return nil, nil, 0, err
	}

	tempDeps := make([][]uint64, len(tasks))

	for i := 0; i <= len(tasks)-1; i++ {
		tempDeps[i] = []uint64{uint64(i), 1}

		for j := range result.AllDeps[i] {
			tempDeps[i] = append(tempDeps[i], uint64(j))
		}
	}

	block.HeaderWithoutCopy().TxDependency = tempDeps

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	p.engine.Finalize(p.bc, header, statedb, block.Transactions(), block.Uncles())

	return receipts, allLogs, *usedGas, nil
}

func (p *ParallelStateProcessorUse) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	var (
		receipts    types.Receipts
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		usedGas     = new(uint64)
		metadata    bool
	)

	// Mutate the block and state according to any hard-fork specs
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}

	tasks := make([]blockstm.ExecTask, 0, len(block.Transactions()))

	shouldDelayFeeCal := true

	coinbase, _ := p.bc.Engine().Author(header)

	deps, delayMap := GetDeps(block.Header().TxDependency)

	if block.Header().TxDependency != nil {
		metadata = true
	}

	for _, j := range delayMap {
		if !j {
			log.Info("BlockSTM", "Dependencies deps", deps)
			log.Info("BlockSTM", "Dependencies delayMap", delayMap)
			log.Info("Going Serial", "!j", !j)
			pSeral := NewStateProcessor(p.config, p.bc, p.engine)
			return pSeral.Process(block, statedb, cfg)
		}
	}

	blockContext := NewEVMBlockContext(header, p.bc, nil)
	// p.bc.Engine().Author(header)
	for i, tx := range block.Transactions() {
		msg, err := tx.AsMessage(types.MakeSigner(p.config, header.Number), header.BaseFee)
		if err != nil {
			log.Error("error creating message", "err", err)
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}

		cleansdb := statedb.Copy()

		if len(header.TxDependency) > 0 {
			shouldDelayFeeCal = delayMap[i]

			task := &ExecutionTask{
				msg:               msg,
				config:            p.config,
				gasLimit:          block.GasLimit(),
				blockNumber:       blockNumber,
				blockHash:         blockHash,
				tx:                tx,
				index:             i,
				cleanStateDB:      cleansdb,
				finalStateDB:      statedb,
				blockChain:        p.bc,
				header:            header,
				evmConfig:         cfg,
				shouldDelayFeeCal: &shouldDelayFeeCal,
				sender:            msg.From(),
				totalUsedGas:      usedGas,
				receipts:          &receipts,
				allLogs:           &allLogs,
				dependencies:      deps[i],
				blockContext:      blockContext,
				coinbase:          coinbase,
			}

			tasks = append(tasks, task)
		} else {
			if msg.From() == coinbase {
				shouldDelayFeeCal = false
			}

			task := &ExecutionTask{
				msg:               msg,
				config:            p.config,
				gasLimit:          block.GasLimit(),
				blockNumber:       blockNumber,
				blockHash:         blockHash,
				tx:                tx,
				index:             i,
				cleanStateDB:      cleansdb,
				finalStateDB:      statedb,
				blockChain:        p.bc,
				header:            header,
				evmConfig:         cfg,
				shouldDelayFeeCal: &shouldDelayFeeCal,
				sender:            msg.From(),
				totalUsedGas:      usedGas,
				receipts:          &receipts,
				allLogs:           &allLogs,
				dependencies:      nil,
				blockContext:      blockContext,
				coinbase:          coinbase,
			}

			tasks = append(tasks, task)
		}
	}

	backupStateDB := statedb.Copy()

	profile := false
	result, err := blockstm.ExecuteParallel(tasks, false, metadata)

	if err == nil && profile {
		_, weight := result.Deps.LongestPath(*result.Stats)

		serialWeight := uint64(0)

		for i := 0; i < len(result.Deps.GetVertices()); i++ {
			serialWeight += (*result.Stats)[i].End - (*result.Stats)[i].Start
		}

		parallelizabilityTimer.Update(time.Duration(serialWeight * 100 / weight))

		log.Info("Parallelizability", "Average (%)", parallelizabilityTimer.Mean())

		log.Info("Parallelizability", "Histogram (%)", parallelizabilityTimer.Percentiles([]float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 0.999, 0.9999}))
	}

	for _, task := range tasks {
		task := task.(*ExecutionTask)
		if task.shouldRerunWithoutFeeDelay {
			shouldDelayFeeCal = false
			*statedb = *backupStateDB

			allLogs = []*types.Log{}
			receipts = types.Receipts{}
			usedGas = new(uint64)

			for _, t := range tasks {
				t := t.(*ExecutionTask)
				t.finalStateDB = backupStateDB
				t.allLogs = &allLogs
				t.receipts = &receipts
				t.totalUsedGas = usedGas
			}

			_, err = blockstm.ExecuteParallel(tasks, false, metadata)

			break
		}
	}

	if err != nil {
		log.Error("blockstm error executing block", "err", err)
		return nil, nil, 0, err
	}

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	p.engine.Finalize(p.bc, header, statedb, block.Transactions(), block.Uncles())

	return receipts, allLogs, *usedGas, nil
}

func GetDepListForTx(txDependency [][]uint64, txIdx int) []int {
	if len(txDependency) == 1 && (txDependency[0][0] == uint64(0) && txDependency[0][1] == uint64(1)) {
		return []int{txIdx, 1}
	}

	tempArr := []int{}
	tempIdx := -1

	for ind, val := range txDependency {
		if int(val[0]) == txIdx {
			tempIdx = ind
			break
		}
	}

	for i := 0; i < len(txDependency[tempIdx]); i++ {
		tempArr = append(tempArr, int(txDependency[tempIdx][i]))
	}

	return tempArr
}

// Jerry's function
func GetDeps(txDependency [][]uint64) (map[int][]int, map[int]bool) {
	deps := make(map[int][]int)
	delayMap := make(map[int]bool)

	for i := 0; i <= len(txDependency)-1; i++ {
		idx := int(txDependency[i][0])
		shouldDelay := txDependency[i][1] == 1

		delayMap[idx] = shouldDelay

		deps[idx] = []int{}

		for j := 2; j <= len(txDependency[i])-1; j++ {
			deps[idx] = append(deps[idx], int(txDependency[i][j]))
		}
	}

	return deps, delayMap
}
