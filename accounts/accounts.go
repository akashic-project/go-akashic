// Copyright 2017 The go-ethereum Authors
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

// Package accounts implements high level Ethereum account management.
// パッケージアカウントは、高レベルのイーサリアムアカウント管理を実装します。
package accounts

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"golang.org/x/crypto/sha3"
)

// Account represents an Ethereum account located at a specific location defined
// by the optional URL field.
// アカウントは、オプションのURLフィールドで定義された特定の場所にあるイーサリアムアカウントを表します。
type Account struct {
	Address common.Address `json:"address"` // キーから派生したイーサリアムアカウントアドレス // Ethereum account address derived from the key
	URL     URL            `json:"url"`     // バックエンド内のオプションのリソースロケーター// Optional resource locator within a backend
}

const (
	MimetypeDataWithValidator = "data/validator"
	MimetypeTypedData         = "data/typed"
	MimetypeClique            = "application/x-clique-header"
	MimetypeTextPlain         = "text/plain"
)

// Wallet represents a software or hardware wallet that might contain one or more
// accounts (derived from the same seed).
// Walletは、（同じシードから派生した）1つ以上のアカウントを含む可能性のあるソフトウェアまたはハードウェアのウォレットを表します。
type Wallet interface {
	// URL retrieves the canonical path under which this wallet is reachable. It is
	// user by upper layers to define a sorting order over all wallets from multiple
	// backends.
	// URLは、このウォレットに到達できる正規のパスを取得します。
	// 複数のバックエンドからのすべてのウォレットの並べ替え順序を定義するのは、上位層ごとのユーザーです。
	URL() URL

	// Status returns a textual status to aid the user in the current state of the
	// wallet. It also returns an error indicating any failure the wallet might have
	// encountered.
	// Statusは、ウォレットの現在の状態でユーザーを支援するためのテキストステータスを返します。
	// また、ウォレットで発生した可能性のある障害を示すエラーも返します。
	Status() (string, error)

	// Open initializes access to a wallet instance. It is not meant to unlock or
	// decrypt account keys, rather simply to establish a connection to hardware
	// wallets and/or to access derivation seeds.
	//
	// The passphrase parameter may or may not be used by the implementation of a
	// particular wallet instance. The reason there is no passwordless open method
	// is to strive towards a uniform wallet handling, oblivious to the different
	// backend providers.
	//
	// Please note, if you open a wallet, you must close it to release any allocated
	// resources (especially important when working with hardware wallets).
	// Openは、ウォレットインスタンスへのアクセスを初期化します。
	// アカウントキーのロックを解除または復号化することを目的としたものではなく、
	// 単にハードウェアウォレットへの接続を確立したり、派生シードにアクセスしたりすることを目的としています。
	//
	// パスフレーズパラメータは、特定のウォレットインスタンスの実装で使用される場合と使用されない場合があります。
	// パスワードなしのオープンな方法がない理由は、さまざまなバックエンドプロバイダーに気付かれずに、
	// 均一なウォレット処理に向けて努力するためです。
	//
	// ウォレットを開く場合は、割り当てられたリソースを解放するためにウォレットを閉じる必要があることに注意してください
	//（特にハードウェアウォレットを操作する場合に重要です）。
	Open(passphrase string) error

	// Close releases any resources held by an open wallet instance.
	// Closeは、開いているウォレットインスタンスによって保持されているすべてのリソースを解放します。
	Close() error

	// Accounts retrieves the list of signing accounts the wallet is currently aware
	// of. For hierarchical deterministic wallets, the list will not be exhaustive,
	// rather only contain the accounts explicitly pinned during account derivation.
	// アカウントは、ウォレットが現在認識している署名アカウントのリストを取得します。
	// 階層的な決定論的ウォレットの場合、リストは網羅的ではなく、アカウントの派生中に明示的に固定されたアカウントのみが含まれます。
	Accounts() []Account

	// Contains returns whether an account is part of this particular wallet or not.
	// アカウントがこの特定のウォレットの一部であるかどうかを返します。
	Contains(account Account) bool

	// Derive attempts to explicitly derive a hierarchical deterministic account at
	// the specified derivation path. If requested, the derived account will be added
	// to the wallet's tracked account list.
	// 派生は、指定された派生パスで階層的な決定論的アカウントを明示的に派生させようとします。
	// 要求された場合、派生アカウントはウォレットの追跡アカウントリストに追加されます。
	Derive(path DerivationPath, pin bool) (Account, error)

	// SelfDerive sets a base account derivation path from which the wallet attempts
	// to discover non zero accounts and automatically add them to list of tracked
	// accounts.
	//
	// Note, self derivation will increment the last component of the specified path
	// opposed to decending into a child path to allow discovering accounts starting
	// from non zero components.
	//
	// Some hardware wallets switched derivation paths through their evolution, so
	// this method supports providing multiple bases to discover old user accounts
	// too. Only the last base will be used to derive the next empty account.
	//
	// You can disable automatic account discovery by calling SelfDerive with a nil
	// chain state reader.
	// SelfDeriveは、ウォレットがゼロ以外のアカウントを検出し、
	// 追跡されたアカウントのリストに自動的に追加しようとするベースアカウントの派生パスを設定します。
	//
	// 自己派生は、子パスに降順するのではなく、指定されたパスの最後のコンポーネントをインクリメントして、
	// ゼロ以外のコンポーネントから始まるアカウントを検出できるようにすることに注意してください。
	//
	// 一部のハードウェアウォレットは、進化を通じて派生パスを切り替えたため、
	// このメソッドは、古いユーザーアカウントを検出するための複数のベースの提供もサポートします。
	// 最後のベースのみが次の空のアカウントを導出するために使用されます。
	//
	// nilチェーン状態リーダーでSelfDeriveを呼び出すことにより、自動アカウント検出を無効にできます。
	SelfDerive(bases []DerivationPath, chain ethereum.ChainStateReader)

	// SignData requests the wallet to sign the hash of the given data
	// It looks up the account specified either solely via its address contained within,
	// or optionally with the aid of any location metadata from the embedded URL field.
	//
	// If the wallet requires additional authentication to sign the request (e.g.
	// a password to decrypt the account, or a PIN code o verify the transaction),
	// an AuthNeededError instance will be returned, containing infos for the user
	// about which fields or actions are needed. The user may retry by providing
	// the needed details via SignDataWithPassphrase, or by other means (e.g. unlock
	// the account in a keystore).
	SignData(account Account, mimeType string, data []byte) ([]byte, error)

	// SignDataWithPassphrase is identical to SignData, but also takes a password
	// NOTE: there's a chance that an erroneous call might mistake the two strings, and
	// supply password in the mimetype field, or vice versa. Thus, an implementation
	// should never echo the mimetype or return the mimetype in the error-response
	SignDataWithPassphrase(account Account, passphrase, mimeType string, data []byte) ([]byte, error)

	// SignText requests the wallet to sign the hash of a given piece of data, prefixed
	// by the Ethereum prefix scheme
	// It looks up the account specified either solely via its address contained within,
	// or optionally with the aid of any location metadata from the embedded URL field.
	//
	// If the wallet requires additional authentication to sign the request (e.g.
	// a password to decrypt the account, or a PIN code o verify the transaction),
	// an AuthNeededError instance will be returned, containing infos for the user
	// about which fields or actions are needed. The user may retry by providing
	// the needed details via SignTextWithPassphrase, or by other means (e.g. unlock
	// the account in a keystore).
	//
	// This method should return the signature in 'canonical' format, with v 0 or 1
	SignText(account Account, text []byte) ([]byte, error)

	// SignTextWithPassphrase is identical to Signtext, but also takes a password
	SignTextWithPassphrase(account Account, passphrase string, hash []byte) ([]byte, error)

	// SignTx requests the wallet to sign the given transaction.
	//
	// It looks up the account specified either solely via its address contained within,
	// or optionally with the aid of any location metadata from the embedded URL field.
	//
	// If the wallet requires additional authentication to sign the request (e.g.
	// a password to decrypt the account, or a PIN code to verify the transaction),
	// an AuthNeededError instance will be returned, containing infos for the user
	// about which fields or actions are needed. The user may retry by providing
	// the needed details via SignTxWithPassphrase, or by other means (e.g. unlock
	// the account in a keystore).
	SignTx(account Account, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)

	// SignTxWithPassphrase is identical to SignTx, but also takes a password
	SignTxWithPassphrase(account Account, passphrase string, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
}

// Backend is a "wallet provider" that may contain a batch of accounts they can
// sign transactions with and upon request, do so.
type Backend interface {
	// Wallets retrieves the list of wallets the backend is currently aware of.
	//
	// The returned wallets are not opened by default. For software HD wallets this
	// means that no base seeds are decrypted, and for hardware wallets that no actual
	// connection is established.
	//
	// The resulting wallet list will be sorted alphabetically based on its internal
	// URL assigned by the backend. Since wallets (especially hardware) may come and
	// go, the same wallet might appear at a different positions in the list during
	// subsequent retrievals.
	Wallets() []Wallet

	// Subscribe creates an async subscription to receive notifications when the
	// backend detects the arrival or departure of a wallet.
	Subscribe(sink chan<- WalletEvent) event.Subscription
}

// TextHash is a helper function that calculates a hash for the given message that can be
// safely used to calculate a signature from.
//
// The hash is calulcated as
//   keccak256("\x19Ethereum Signed Message:\n"${message length}${message}).
//
// This gives context to the signed message and prevents signing of transactions.
func TextHash(data []byte) []byte {
	hash, _ := TextAndHash(data)
	return hash
}

// TextAndHash is a helper function that calculates a hash for the given message that can be
// safely used to calculate a signature from.
//
// The hash is calulcated as
//   keccak256("\x19Ethereum Signed Message:\n"${message length}${message}).
//
// This gives context to the signed message and prevents signing of transactions.
func TextAndHash(data []byte) ([]byte, string) {
	msg := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(data), string(data))
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write([]byte(msg))
	return hasher.Sum(nil), msg
}

// WalletEventType represents the different event types that can be fired by
// the wallet subscription subsystem.
type WalletEventType int

const (
	// WalletArrived is fired when a new wallet is detected either via USB or via
	// a filesystem event in the keystore.
	// WalletArrivedは、USBまたはキーストアのファイルシステムイベントを介して新しいウォレットが検出されたときに発生します。
	WalletArrived WalletEventType = iota

	// WalletOpened is fired when a wallet is successfully opened with the purpose
	// of starting any background processes such as automatic key derivation.
	// WalletOpenedは、自動キー導出などのバックグラウンドプロセスを開始する目的でウォレットが正常に開かれたときに発生します。
	WalletOpened

	// WalletDropped
	WalletDropped
)

// WalletEvent is an event fired by an account backend when a wallet arrival or
// departure is detected.
// WalletEventは、ウォレットの到着または出発が検出されたときにアカウントバックエンドによって発生するイベントです。
type WalletEvent struct {
	Wallet Wallet          // ウォレットインスタンスが到着または出発しました // Wallet instance arrived or departed
	Kind   WalletEventType // システムで発生したイベントタイプ             // Event type that happened in the system
}
