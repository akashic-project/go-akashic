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

package bind

import (
	"context"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	// ErrNoCode is returned by call and transact operations for which the requested
	// recipient contract to operate on does not exist in the state db or does not
	// have any code associated with it (i.e. suicided).
	// ErrNoCodeは、操作するように要求された受信者コントラクトが状態dbに存在しないか、
	// それに関連付けられたコードがない（つまり、自殺した）呼び出しおよびトランザクション操作によって返されます。
	ErrNoCode = errors.New("no contract code at given address") // 指定された住所に契約コードがありません

	// ErrNoPendingState is raised when attempting to perform a pending state action
	// on a backend that doesn't implement PendingContractCaller.
	// ErrNoPendingStateは、PendingContractCallerを実装していないバックエンドで保留状態のアクションを実行しようとすると発生します。
	ErrNoPendingState = errors.New("backend does not support pending state") // バックエンドは保留状態をサポートしていません

	// ErrNoCodeAfterDeploy is returned by WaitDeployed if contract creation leaves
	// an empty contract behind.
	// コントラクトの作成により空のコントラクトが残された場合、ErrNoCodeAfterDeployはWaitDeployedによって返されます。
	ErrNoCodeAfterDeploy = errors.New("no contract code after deployment") // 展開後の契約コードなし
)

// ContractCaller defines the methods needed to allow operating with a contract on a read
// only basis.
// ContractCallerは、読み取り専用でコントラクトを操作できるようにするために必要なメソッドを定義します。
type ContractCaller interface {
	// CodeAt returns the code of the given account. This is needed to differentiate
	// between contract internal errors and the local chain being out of sync.
	// CodeAtは、指定されたアカウントのコードを返します。
	// これは、コントラクトの内部エラーとローカルチェーンが同期していないことを区別するために必要です。
	CodeAt(ctx context.Context, contract common.Address, blockNumber *big.Int) ([]byte, error)

	// CallContract executes an Ethereum contract call with the specified data as the
	// input.
	// CallContractは、指定されたデータを入力としてイーサリアムコントラクト呼び出しを実行します。
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
}

// PendingContractCaller defines methods to perform contract calls on the pending state.
// Call will try to discover this interface when access to the pending state is requested.
// If the backend does not support the pending state, Call returns ErrNoPendingState.
// PresidentingContractCallerは、保留状態でコントラクト呼び出しを実行するメソッドを定義します。
// 保留状態へのアクセスが要求されると、Callはこのインターフェースを検出しようとします。
// バックエンドが保留状態をサポートしていない場合、CallはErrNoPendingStateを返します。
type PendingContractCaller interface {
	// PendingCodeAt returns the code of the given account in the pending state.
	// PendingCodeAtは保留状態の指定されたアカウントのコードを返します。
	PendingCodeAt(ctx context.Context, contract common.Address) ([]byte, error)

	// PendingCallContract executes an Ethereum contract call against the pending state.
	// PendingCallContractは保留状態に対してEthereumコントラクトコールを実行します。
	PendingCallContract(ctx context.Context, call ethereum.CallMsg) ([]byte, error)
}

// ContractTransactor defines the methods needed to allow operating with a contract
// on a write only basis. Besides the transacting method, the remainder are helpers
// used when the user does not provide some needed values, but rather leaves it up
// to the transactor to decide.
// ContractTransactorは、書き込みのみでコントラクトを操作できるようにするために必要なメソッドを定義します。
// トランザクション方式に加えて、残りは、ユーザーが必要な値を提供しない場合に使用されるヘルパーですが、
// 決定するのはトランザクション担当者に任されています。
type ContractTransactor interface {
	// HeaderByNumber returns a block header from the current canonical chain. If
	// number is nil, the latest known header is returned.
	// HeaderByNumberは、現在の正規チェーンからブロックヘッダーを返します。
	// numberがnilの場合、最新の既知のヘッダーが返されます。
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)

	// PendingCodeAt returns the code of the given account in the pending state.
	// PendingCodeAtは保留状態の指定されたアカウントのコードを返します。
	PendingCodeAt(ctx context.Context, account common.Address) ([]byte, error)

	// PendingNonceAt retrieves the current pending nonce associated with an account.
	// PresidentingNonceAtは、アカウントに関連付けられている現在保留中のナンスを取得します。
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)

	// SuggestGasPrice retrieves the currently suggested gas price to allow a timely
	// execution of a transaction.
	// SuggestGasPriceは、現在提案されているガス価格を取得して、
	// トランザクションをタイムリーに実行できるようにします。
	SuggestGasPrice(ctx context.Context) (*big.Int, error)

	// SuggestGasTipCap retrieves the currently suggested 1559 priority fee to allow
	// a timely execution of a transaction.
	// SuggestGasTipCapは、トランザクションのタイムリーな実行を可能にするために、
	// 現在提案されている1559優先料金を取得します。
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)

	// EstimateGas tries to estimate the gas needed to execute a specific
	// transaction based on the current pending state of the backend blockchain.
	// There is no guarantee that this is the true gas limit requirement as other
	// transactions may be added or removed by miners, but it should provide a basis
	// for setting a reasonable default.
	// EstimateGasは、バックエンドブロックチェーンの現在の保留状態に基づいて、
	// 特定のトランザクションを実行するために必要なガスを推定しようとします。
	// 他のトランザクションが鉱夫によって追加または削除される可能性があるため、
	// これが真のガス制限要件であるという保証はありませんが、
	// 合理的なデフォルトを設定するための基礎を提供する必要があります。
	EstimateGas(ctx context.Context, call ethereum.CallMsg) (gas uint64, err error)

	// SendTransaction injects the transaction into the pending pool for execution.
	// SendTransactionは、実行のためにトランザクションを保留中のプールに挿入します。
	SendTransaction(ctx context.Context, tx *types.Transaction) error
}

// ContractFilterer defines the methods needed to access log events using one-off
// queries or continuous event subscriptions.
// ContractFiltererは、1回限りのクエリまたは継続的なイベントサブスクリプションを使用して
// ログイベントにアクセスするために必要なメソッドを定義します。
type ContractFilterer interface {
	// FilterLogs executes a log filter operation, blocking during execution and
	// returning all the results in one batch.
	//
	// TODO(karalabe): Deprecate when the subscription one can return past data too.
	// FilterLogsはログフィルター操作を実行し、実行中にブロックし、すべての結果を1つのバッチで返します。
	//
	// TODO（karalabe）：サブスクリプションが過去のデータも返すことができる場合は非推奨にします。
	FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error)

	// SubscribeFilterLogs creates a background log filtering operation, returning
	// a subscription immediately, which can be used to stream the found events.
	// SubscribeFilterLogsは、バックグラウンドログフィルタリング操作を作成し、サブスクリプションをすぐに返します。
	// これを使用して、見つかったイベントをストリーミングできます。
	SubscribeFilterLogs(ctx context.Context, query ethereum.FilterQuery, ch chan<- types.Log) (ethereum.Subscription, error)
}

// DeployBackend wraps the operations needed by WaitMined and WaitDeployed.
// DeployBackendは、WaitMinedとWaitDeployedに必要な操作をラップします。
type DeployBackend interface {
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
	CodeAt(ctx context.Context, account common.Address, blockNumber *big.Int) ([]byte, error)
}

// ContractBackend defines the methods needed to work with contracts on a read-write basis.
// ContractBackendは、読み取り/書き込みベースでコントラクトを操作するために必要なメソッドを定義します。
type ContractBackend interface {
	ContractCaller
	ContractTransactor
	ContractFilterer
}
