// Copyright 2014 The go-ethereum Authors
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
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

var emptyCodeHash = crypto.Keccak256Hash(nil)

/*
The State Transitioning Model

A state transition is a change made when a transaction is applied to the current world state
The state transitioning model does all the necessary work to work out a valid new state root.

1) Nonce handling
2) Pre pay gas
3) Create a new state object if the recipient is \0*32
4) Value transfer
== If contract creation ==
  4a) Attempt to run transaction data
  4b) If valid, use result as code for the new state object
== end ==
5) Run Script section
6) Derive new state root
*/
/*
状態遷移モデル

状態遷移は、トランザクションが現在の世界の状態に適用されるときに行われる変更です。
状態遷移モデルは、有効な新しい状態ルートを作成するために必要なすべての作業を実行します。

1）ノンス処理
2）前払いガス
3）受信者が\ 0 * 32の場合、新しい状態オブジェクトを作成します
4）価値移転
==契約作成の場合==
   4a）トランザクションデータの実行を試みます
   4b）有効な場合は、結果を新しい状態オブジェクトのコードとして使用します
==終了==
5）スクリプトセクションを実行する
6）新しい状態ルートを導出する
*/
type StateTransition struct {
	gp         *GasPool
	msg        Message
	gas        uint64
	gasPrice   *big.Int
	gasFeeCap  *big.Int
	gasTipCap  *big.Int
	initialGas uint64
	value      *big.Int
	data       []byte
	state      vm.StateDB
	evm        *vm.EVM
}

// Message represents a message sent to a contract.
// メッセージは、契約に送信されたメッセージを表します。
type Message interface {
	From() common.Address
	To() *common.Address

	GasPrice() *big.Int
	GasFeeCap() *big.Int
	GasTipCap() *big.Int
	Gas() uint64
	Value() *big.Int

	Nonce() uint64
	IsFake() bool
	Data() []byte
	AccessList() types.AccessList
}

// ExecutionResult includes all output after executing given evm
// message no matter the execution itself is successful or not.
// ExecutionResultには、実行自体が成功したかどうかに関係なく、
// 指定されたevmメッセージの実行後のすべての出力が含まれます。
type ExecutionResult struct {
	UsedGas    uint64 // 使用済みガスの合計ですが、返金されたガスを含みます                  // Total used gas but include the refunded gas
	Err        error  // 実行中に発生したエラー（core/vm/errors.goにリストされています）// Any error encountered during the execution(listed in core/vm/errors.go)
	ReturnData []byte // evmから返されたデータ（関数の結果またはrevert opcodeで提供されたデータ） // Returned data from evm(function result or data supplied with revert opcode)
}

// Unwrap returns the internal evm error which allows us for further
// analysis outside.
// Unwrapは内部evmエラーを返します。これにより、外部でさらに分析することができます。
func (result *ExecutionResult) Unwrap() error {
	return result.Err
}

// Failed returns the indicator whether the execution is successful or not
// Failedは、実行が成功したかどうかのインジケーターを返します
func (result *ExecutionResult) Failed() bool { return result.Err != nil }

// Return is a helper function to help caller distinguish between revert reason
// and function return. Return returns the data after execution if no error occurs.
// Returnは、呼び出し元が復帰理由と関数returnを区別するのに役立つヘルパー関数です。
// エラーが発生しなかった場合、Returnは実行後にデータを返します。
func (result *ExecutionResult) Return() []byte {
	if result.Err != nil {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// Revert returns the concrete revert reason if the execution is aborted by `REVERT`
// opcode. Note the reason can be nil if no data supplied with revert opcode.
// 実行が `REVERT`オペコードによって中止された場合、Revertは具体的な復帰理由を返します。
// revert opcodeでデータが提供されていない場合、理由はnilになる可能性があることに注意してください。
func (result *ExecutionResult) Revert() []byte {
	if result.Err != vm.ErrExecutionReverted {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// IntrinsicGas computes the 'intrinsic gas' for a message with the given data.
// IntrinsicGasは、指定されたデータを使用してメッセージの「固有ガス」を計算します。
func IntrinsicGas(data []byte, accessList types.AccessList, isContractCreation bool, isHomestead, isEIP2028 bool) (uint64, error) {
	// Set the starting gas for the raw transaction
	// 生のトランザクションの開始ガスを設定します
	var gas uint64
	if isContractCreation && isHomestead {
		gas = params.TxGasContractCreation
	} else {
		gas = params.TxGas
	}
	// Bump the required gas by the amount of transactional data
	// トランザクションデータの量だけ必要なガスをバンプします
	if len(data) > 0 {
		// Zero and non-zero bytes are priced differently
		// ゼロバイトと非ゼロバイトの価格は異なります
		var nz uint64
		for _, byt := range data {
			if byt != 0 {
				nz++
			}
		}
		// Make sure we don't exceed uint64 for all data combinations
		// すべてのデータの組み合わせでuint64を超えないようにしてください
		nonZeroGas := params.TxDataNonZeroGasFrontier
		if isEIP2028 {
			nonZeroGas = params.TxDataNonZeroGasEIP2028
		}
		if (math.MaxUint64-gas)/nonZeroGas < nz {
			return 0, ErrGasUintOverflow
		}
		gas += nz * nonZeroGas

		z := uint64(len(data)) - nz
		if (math.MaxUint64-gas)/params.TxDataZeroGas < z {
			return 0, ErrGasUintOverflow
		}
		gas += z * params.TxDataZeroGas
	}
	if accessList != nil {
		gas += uint64(len(accessList)) * params.TxAccessListAddressGas
		gas += uint64(accessList.StorageKeys()) * params.TxAccessListStorageKeyGas
	}
	return gas, nil
}

// NewStateTransition initialises and returns a new state transition object.
// NewStateTransitionは、新しい状態遷移オブジェクトを初期化して返します。
func NewStateTransition(evm *vm.EVM, msg Message, gp *GasPool) *StateTransition {
	return &StateTransition{
		gp:        gp,
		evm:       evm,
		msg:       msg,
		gasPrice:  msg.GasPrice(),
		gasFeeCap: msg.GasFeeCap(),
		gasTipCap: msg.GasTipCap(),
		value:     msg.Value(),
		data:      msg.Data(),
		state:     evm.StateDB,
	}
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any EVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
// ApplyMessageは、環境内の古い状態に対して指定されたメッセージを適用することにより、
// 新しい状態を計算します。
//
// ApplyMessageは、EVMの実行によって返されたバイト（発生した場合）、
// 使用されたガス（ガスの払い戻しを含む）、および失敗した場合のエラーを返します。
// エラーは常にコアエラーを示します。
// これは、メッセージがその特定の状態では常に失敗し、ブロック内で受け入れられないことを意味します。
func ApplyMessage(evm *vm.EVM, msg Message, gp *GasPool) (*ExecutionResult, error) {
	return NewStateTransition(evm, msg, gp).TransitionDb()
}

// to returns the recipient of the message.
// メッセージの受信者を返します。
func (st *StateTransition) to() common.Address {
	if st.msg == nil || st.msg.To() == nil /* 契約の作成 */ /* contract creation */ {
		return common.Address{}
	}
	return *st.msg.To()
}

func (st *StateTransition) buyGas() error {
	mgval := new(big.Int).SetUint64(st.msg.Gas())
	mgval = mgval.Mul(mgval, st.gasPrice)
	balanceCheck := mgval
	if st.gasFeeCap != nil {
		balanceCheck = new(big.Int).SetUint64(st.msg.Gas())
		balanceCheck = balanceCheck.Mul(balanceCheck, st.gasFeeCap)
		balanceCheck.Add(balanceCheck, st.value)
	}
	if have, want := st.state.GetBalance(st.msg.From()), balanceCheck; have.Cmp(want) < 0 {
		return fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, st.msg.From().Hex(), have, want)
	}
	if err := st.gp.SubGas(st.msg.Gas()); err != nil {
		return err
	}
	st.gas += st.msg.Gas()

	st.initialGas = st.msg.Gas()
	st.state.SubBalance(st.msg.From(), mgval)
	return nil
}

func (st *StateTransition) preCheck() error {
	// Only check transactions that are not fake
	// 偽物ではないトランザクションのみをチェックします
	if !st.msg.IsFake() {
		// Make sure this transaction's nonce is correct.
		// このトランザクションのナンスが正しいことを確認します。
		stNonce := st.state.GetNonce(st.msg.From())
		if msgNonce := st.msg.Nonce(); stNonce < msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooHigh,
				st.msg.From().Hex(), msgNonce, stNonce)
		} else if stNonce > msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooLow,
				st.msg.From().Hex(), msgNonce, stNonce)
		} else if stNonce+1 < stNonce {
			return fmt.Errorf("%w: address %v, nonce: %d", ErrNonceMax,
				st.msg.From().Hex(), stNonce)
		}
		// Make sure the sender is an EOA
		// 送信者がEOAであることを確認します
		if codeHash := st.state.GetCodeHash(st.msg.From()); codeHash != emptyCodeHash && codeHash != (common.Hash{}) {
			return fmt.Errorf("%w: address %v, codehash: %s", ErrSenderNoEOA,
				st.msg.From().Hex(), codeHash)
		}
	}
	// Make sure that transaction gasFeeCap is greater than the baseFee (post london)
	// トランザクションgasFeeCapがbaseFee（post london）よりも大きいことを確認します
	if st.evm.ChainConfig().IsLondon(st.evm.Context.BlockNumber) {
		// Skip the checks if gas fields are zero and baseFee was explicitly disabled (eth_call)
		// ガス田がゼロで、baseFeeが明示的に無効にされているかどうかのチェックをスキップします（eth_call）
		if !st.evm.Config.NoBaseFee || st.gasFeeCap.BitLen() > 0 || st.gasTipCap.BitLen() > 0 {
			if l := st.gasFeeCap.BitLen(); l > 256 {
				return fmt.Errorf("%w: address %v, maxFeePerGas bit length: %d", ErrFeeCapVeryHigh,
					st.msg.From().Hex(), l)
			}
			if l := st.gasTipCap.BitLen(); l > 256 {
				return fmt.Errorf("%w: address %v, maxPriorityFeePerGas bit length: %d", ErrTipVeryHigh,
					st.msg.From().Hex(), l)
			}
			if st.gasFeeCap.Cmp(st.gasTipCap) < 0 {
				return fmt.Errorf("%w: address %v, maxPriorityFeePerGas: %s, maxFeePerGas: %s", ErrTipAboveFeeCap,
					st.msg.From().Hex(), st.gasTipCap, st.gasFeeCap)
			}
			// This will panic if baseFee is nil, but basefee presence is verified
			// as part of header validation.
			// baseFeeがnilの場合、これはパニックになりますが、
			// basefeeの存在はヘッダー検証の一部として検証されます。
			if st.gasFeeCap.Cmp(st.evm.Context.BaseFee) < 0 {
				return fmt.Errorf("%w: address %v, maxFeePerGas: %s baseFee: %s", ErrFeeCapTooLow,
					st.msg.From().Hex(), st.gasFeeCap, st.evm.Context.BaseFee)
			}
		}
	}
	return st.buyGas()
}

// TransitionDb will transition the state by applying the current message and
// returning the evm execution result with following fields.
//
// - used gas:
//      total gas used (including gas being refunded)
// - returndata:
//      the returned data from evm
// - concrete execution error:
//      various **EVM** error which aborts the execution,
//      e.g. ErrOutOfGas, ErrExecutionReverted
//
// However if any consensus issue encountered, return the error directly with
// nil evm execution result.
// TransitionDbは、現在のメッセージを適用し、次のフィールドでevm実行結果を返すことにより、状態を遷移します。
//
// -使用済みガス：
// 使用されたガスの合計（返金されるガスを含む）
// --returndata：
// evmから返されたデータ
// -具体的な実行エラー：
// 実行を中止するさまざまな** EVM **エラー、
// 例： ErrOutOfGas、ErrExecutionReverted
//
// ただし、コンセンサスの問題が発生した場合は、nilevmの実行結果とともにエラーを直接返します。
func (st *StateTransition) TransitionDb() (*ExecutionResult, error) {
	// First check this message satisfies all consensus rules before
	// applying the message. The rules include these clauses
	//
	// 1. the nonce of the message caller is correct
	// 2. caller has enough balance to cover transaction fee(gaslimit * gasprice)
	// 3. the amount of gas required is available in the block
	// 4. the purchased gas is enough to cover intrinsic usage
	// 5. there is no overflow when calculating intrinsic gas
	// 6. caller has enough balance to cover asset transfer for **topmost** call

	// Check clauses 1-3, buy gas if everything is correct

	// メッセージを適用する前に、まずこのメッセージがすべてのコンセンサスルールを満たしていることを確認してください。
	// ルールにはこれらの条項が含まれます
	//
	// 1.メッセージ発信者のナンスは正しい
	// 2.発信者は、取引手数料をカバーするのに十分な残高があります（gaslimit * gasprice）
	// 3.必要なガスの量はブロックで利用可能です
	// 4.購入したガスは、固有の使用量をカバーするのに十分です
	// 5.。固有ガスの計算時にオーバーフローはありません
	// 6.発信者には、**最上位**の通話のアセット転送をカバーするのに十分な残高があります

	//条項1〜3を確認し、すべてが正しい場合はガスを購入します
	if err := st.preCheck(); err != nil {
		return nil, err
	}
	msg := st.msg
	sender := vm.AccountRef(msg.From())
	homestead := st.evm.ChainConfig().IsHomestead(st.evm.Context.BlockNumber)
	istanbul := st.evm.ChainConfig().IsIstanbul(st.evm.Context.BlockNumber)
	london := st.evm.ChainConfig().IsLondon(st.evm.Context.BlockNumber)
	contractCreation := msg.To() == nil

	// Check clauses 4-5, subtract intrinsic gas if everything is correct
	// 4〜5項を確認し、すべてが正しければ固有ガスを差し引く
	gas, err := IntrinsicGas(st.data, st.msg.AccessList(), contractCreation, homestead, istanbul)
	if err != nil {
		return nil, err
	}
	if st.gas < gas {
		return nil, fmt.Errorf("%w: have %d, want %d", ErrIntrinsicGas, st.gas, gas)
	}
	st.gas -= gas

	// Check clause 6
	// 句6を確認します
	if msg.Value().Sign() > 0 && !st.evm.Context.CanTransfer(st.state, msg.From(), msg.Value()) {
		return nil, fmt.Errorf("%w: address %v", ErrInsufficientFundsForTransfer, msg.From().Hex())
	}

	// Set up the initial access list.
	// 初期アクセスリストを設定します。
	if rules := st.evm.ChainConfig().Rules(st.evm.Context.BlockNumber); rules.IsBerlin {
		st.state.PrepareAccessList(msg.From(), msg.To(), vm.ActivePrecompiles(rules), msg.AccessList())
	}
	var (
		ret   []byte
		vmerr error // vmエラーはコンセンサスに影響を与えないため、errに割り当てられません // vm errors do not effect consensus and are therefore not assigned to err
	)
	if contractCreation {
		ret, _, st.gas, vmerr = st.evm.Create(sender, st.data, st.gas, st.value)
	} else {
		//次のトランザクションのためにナンスをインクリメントします // Increment the nonce for the next transaction
		st.state.SetNonce(msg.From(), st.state.GetNonce(sender.Address())+1)
		ret, st.gas, vmerr = st.evm.Call(sender, st.to(), st.data, st.gas, st.value)
	}

	if !london {
		// Before EIP-3529: refunds were capped to gasUsed / 2
		// EIP-3529より前：払い戻しはgasUsed / 2に制限されていました
		st.refundGas(params.RefundQuotient)
	} else {
		// After EIP-3529: refunds are capped to gasUsed / 5
		// EIP-3529の後：払い戻しはgasUsed / 5に制限されます
		st.refundGas(params.RefundQuotientEIP3529)
	}
	effectiveTip := st.gasPrice
	if london {
		effectiveTip = cmath.BigMin(st.gasTipCap, new(big.Int).Sub(st.gasFeeCap, st.evm.Context.BaseFee))
	}
	st.state.AddBalance(st.evm.Context.Coinbase, new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), effectiveTip))

	return &ExecutionResult{
		UsedGas:    st.gasUsed(),
		Err:        vmerr,
		ReturnData: ret,
	}, nil
}

func (st *StateTransition) refundGas(refundQuotient uint64) {
	// Apply refund counter, capped to a refund quotient
	refund := st.gasUsed() / refundQuotient
	if refund > st.state.GetRefund() {
		refund = st.state.GetRefund()
	}
	st.gas += refund

	// Return ETH for remaining gas, exchanged at the original rate.
	remaining := new(big.Int).Mul(new(big.Int).SetUint64(st.gas), st.gasPrice)
	st.state.AddBalance(st.msg.From(), remaining)

	// Also return remaining gas to the block gas counter so it is
	// available for the next transaction.
	st.gp.AddGas(st.gas)
}

// gasUsed returns the amount of gas used up by the state transition.
func (st *StateTransition) gasUsed() uint64 {
	return st.initialGas - st.gas
}
