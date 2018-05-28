// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package vm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/vm/evm"
	"github.com/vechain/thor/vm/statedb"
)

// Config is ref to evm.Config.
type Config evm.Config

// Output contains the execution return value.
type Output struct {
	Data            []byte
	Events          []*Event
	Transfers       []*Transfer
	LeftOverGas     uint64
	RefundGas       uint64
	Preimages       map[thor.Bytes32][]byte
	VMErr           error         // VMErr identify the execution result of the contract function, not evm function's err.
	ContractAddress *thor.Address // if create a new contract, or is nil.
}

// Event represents a contract log event. These events are generated by the LOG opcode and
// stored/indexed by the node.
type Event struct {
	// address of the contract that generated the event
	Address thor.Address
	// list of topics provided by the contract.
	Topics []thor.Bytes32
	// supplied by the contract, usually ABI-encoded
	Data []byte
}

// Transfer represents token transfer.
type Transfer struct {
	Sender    thor.Address
	Recipient thor.Address
	Amount    *big.Int
}

// State to decouple with state.State
type State interface {
	statedb.State
	GetEnergy(thor.Address, uint64) *big.Int
	SetEnergy(thor.Address, *big.Int, uint64)
}

// VM is a facade for ethEvm.
type VM struct {
	evm     *evm.EVM
	stateDB *statedb.StateDB
}

var chainConfig = &params.ChainConfig{
	ChainId:        big.NewInt(0),
	HomesteadBlock: big.NewInt(0),
	DAOForkBlock:   big.NewInt(0),
	DAOForkSupport: false,
	EIP150Block:    big.NewInt(0),
	EIP150Hash:     common.Hash{},
	EIP155Block:    big.NewInt(0),
	EIP158Block:    big.NewInt(0),
	ByzantiumBlock: big.NewInt(0),
	Ethash:         nil,
	Clique:         nil,
}

// Context for VM runtime.
type Context struct {
	Origin      thor.Address
	Beneficiary thor.Address
	BlockNumber uint32
	Time        uint64
	GasLimit    uint64
	GasPrice    *big.Int
	TxID        thor.Bytes32
	ClauseIndex uint32
	GetHash     func(uint32) thor.Bytes32

	InterceptContractCall evm.InterceptContractCall
	OnCreateContract      evm.OnCreateContract
	OnSuicideContract     evm.OnSuicideContract
}

// The only purpose of this func separate definition is to be compatible with evm.context.
func canTransfer(db evm.StateDB, addr common.Address, amount *big.Int) bool {
	return db.GetBalance(addr).Cmp(amount) >= 0
}

// The only purpose of this func separate definition is to be compatible with evm.Context.
func transfer(db evm.StateDB, sender, recipient common.Address, amount *big.Int) {
	db.SubBalance(sender, amount)
	db.AddBalance(recipient, amount)
}

// New retutrns a new EVM . The returned EVM is not thread safe and should
// only ever be used *once*.
func New(ctx Context, state State, vmConfig Config) (vm *VM) {
	stateDB := statedb.New(state)
	evmCtx := evm.Context{
		CanTransfer: canTransfer,
		Transfer: func(db evm.StateDB, sender, recipient common.Address, amount *big.Int) {
			if amount.Sign() == 0 {
				return
			}
			// touch energy balance when token balance changed
			// SHOULD be performed before transfer
			state.SetEnergy(thor.Address(sender),
				state.GetEnergy(thor.Address(sender), ctx.Time), ctx.Time)
			state.SetEnergy(thor.Address(recipient),
				state.GetEnergy(thor.Address(recipient), ctx.Time), ctx.Time)

			transfer(db, sender, recipient, amount)

			stateDB.AddTransfer(&statedb.Transfer{
				Sender:    thor.Address(sender),
				Recipient: thor.Address(recipient),
				Amount:    amount,
			})
		},
		GetHash: func(n uint64) common.Hash {
			return common.Hash(ctx.GetHash(uint32(n)))
		},
		Difficulty: new(big.Int),

		Origin:      common.Address(ctx.Origin),
		Coinbase:    common.Address(ctx.Beneficiary),
		BlockNumber: new(big.Int).SetUint64(uint64(ctx.BlockNumber)),
		Time:        new(big.Int).SetUint64(ctx.Time),
		GasLimit:    ctx.GasLimit,
		GasPrice:    ctx.GasPrice,
		TxID:        ctx.TxID,
		ClauseIndex: ctx.ClauseIndex,

		InterceptContractCall: ctx.InterceptContractCall,
		OnCreateContract:      ctx.OnCreateContract,
		OnSuicideContract:     ctx.OnSuicideContract,
	}
	return &VM{
		evm.NewEVM(evmCtx, stateDB, chainConfig, evm.Config(vmConfig)),
		stateDB,
	}
}

// Cancel cancels any running EVM operation.
// This may be called concurrently and it's safe to be called multiple times.
func (vm *VM) Cancel() {
	vm.evm.Cancel()
}

// Call executes the contract associated with the addr with the given input as parameters.
// It also handles any necessary value transfer required and takes the necessary steps to
// create accounts and reverses the state in case of an execution error or failed value transfer.
func (vm *VM) Call(caller thor.Address, addr thor.Address, input []byte, gas uint64, value *big.Int) *Output {
	ret, leftOverGas, vmErr := vm.evm.Call(evm.AccountRef(caller), common.Address(addr), input, gas, value)
	events, transfers, preimages := vm.extractStateDBOutputs()
	return &Output{ret, events, transfers, leftOverGas, vm.stateDB.GetRefund(), preimages, vmErr, nil}
}

// StaticCall executes the contract associated with the addr with the given input as parameters
// while disallowing any modifications to the state during the call.
//
// Opcodes that attempt to perform such modifications will result in exceptions instead of performing
// the modifications.
func (vm *VM) StaticCall(caller thor.Address, addr thor.Address, input []byte, gas uint64) *Output {
	ret, leftOverGas, vmErr := vm.evm.StaticCall(evm.AccountRef(caller), common.Address(addr), input, gas)
	events, transfers, preimages := vm.extractStateDBOutputs()
	return &Output{ret, events, transfers, leftOverGas, vm.stateDB.GetRefund(), preimages, vmErr, nil}
}

// Create creates a new contract using code as deployment code.
func (vm *VM) Create(caller thor.Address, code []byte, gas uint64, value *big.Int) *Output {
	ret, contractAddr, leftOverGas, vmErr := vm.evm.Create(evm.AccountRef(caller), code, gas, value)
	contractAddress := thor.Address(contractAddr)
	events, transfers, preimages := vm.extractStateDBOutputs()
	return &Output{ret, events, transfers, leftOverGas, vm.stateDB.GetRefund(), preimages, vmErr, &contractAddress}
}

// ChainConfig returns the evmironment's chain configuration
func (vm *VM) ChainConfig() *params.ChainConfig {
	return vm.evm.ChainConfig()
}

func (vm *VM) extractStateDBOutputs() (
	events []*Event,
	transfers []*Transfer,
	preimages map[thor.Bytes32][]byte,
) {
	vm.stateDB.GetOutputs(
		func(log *types.Log) bool {
			events = append(events, ethlogToEvent(log))
			return true
		},
		func(transfer *statedb.Transfer) bool {
			transfers = append(transfers, (*Transfer)(transfer))
			return true
		},
		func(key common.Hash, value []byte) bool {
			// create on-demand
			if preimages == nil {
				preimages = make(map[thor.Bytes32][]byte)
			}
			preimages[thor.Bytes32(key)] = value
			return true
		},
	)
	return
}

func ethlogToEvent(ethlog *types.Log) *Event {
	var topics []thor.Bytes32
	if len(ethlog.Topics) > 0 {
		topics = make([]thor.Bytes32, 0, len(ethlog.Topics))
		for _, t := range ethlog.Topics {
			topics = append(topics, thor.Bytes32(t))
		}
	}
	return &Event{
		thor.Address(ethlog.Address),
		topics,
		ethlog.Data,
	}
}
