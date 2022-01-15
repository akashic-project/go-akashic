// Copyright 2018 The go-ethereum Authors
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

package scwallet

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	pcsc "github.com/gballet/go-libpcsclite"
	"github.com/status-im/keycard-go/derivationpath"
)

// ErrPairingPasswordNeeded is returned if opening the smart card requires pairing with a pairing
// password. In this case, the calling application should request user input to enter
// the pairing password and send it back.

// スマートカードを開くときにペアリングパスワードとのペアリングが必要な場合は、ErrPairingPasswordNeededが返されます。
// この場合、呼び出し元のアプリケーションは、ペアリングパスワードを入力して送り返すために、ユーザー入力を要求する必要があります。
var ErrPairingPasswordNeeded = errors.New("smartcard: pairing password needed")

// ErrPINNeeded is returned if opening the smart card requires a PIN code. In
// this case, the calling application should request user input to enter the PIN
// and send it back.
// スマートカードを開くためにPINコードが必要な場合は、ErrPINNeededが返されます。
// この場合、呼び出し元のアプリケーションは、PINを入力して送り返すためのユーザー入力を要求する必要があります。
var ErrPINNeeded = errors.New("smartcard: pin needed")

// ErrPINUnblockNeeded is returned if opening the smart card requires a PIN code,
// but all PIN attempts have already been exhausted. In this case the calling
// application should request user input for the PUK and a new PIN code to set
// fo the card.
// スマートカードを開くためにPINコードが必要な場合、ErrPINUnblockNeededが返されますが、すべてのPINの試行はすでに終了しています。
//この場合、呼び出し元のアプリケーションは、PUKのユーザー入力と、カードを設定するための新しいPINコードを要求する必要があります。
var ErrPINUnblockNeeded = errors.New("smartcard: pin unblock needed")

// ErrAlreadyOpen is returned if the smart card is attempted to be opened, but
// there is already a paired and unlocked session.
// スマートカードを開こうとするとErrAlreadyOpenが返されますが、ペアリングされてロック解除されたセッションがすでに存在します。
var ErrAlreadyOpen = errors.New("smartcard: already open")

// ErrPubkeyMismatch is returned if the public key recovered from a signature
// does not match the one expected by the user.
// 署名から復元された公開鍵がユーザーが期待するものと一致しない場合、ErrPubkeyMismatchが返されます。
var ErrPubkeyMismatch = errors.New("smartcard: recovered public key mismatch")

var (
	appletAID = []byte{0xA0, 0x00, 0x00, 0x08, 0x04, 0x00, 0x01, 0x01, 0x01}
	// DerivationSignatureHash is used to derive the public key from the signature of this hash
	// DerivationSignatureHashは、このハッシュの署名から公開鍵を導出するために使用されます
	DerivationSignatureHash = sha256.Sum256(common.Hash{}.Bytes())
)

// List of APDU command-related constants
// APDUコマンド関連の定数のリスト
const (
	claISO7816  = 0
	claSCWallet = 0x80

	insSelect      = 0xA4
	insGetResponse = 0xC0
	sw1GetResponse = 0x61
	sw1Ok          = 0x90

	insVerifyPin  = 0x20
	insUnblockPin = 0x22
	insExportKey  = 0xC2
	insSign       = 0xC0
	insLoadKey    = 0xD0
	insDeriveKey  = 0xD1
	insStatus     = 0xF2
)

// List of ADPU command parameters
// ADPUコマンドパラメータのリスト
const (
	P1DeriveKeyFromMaster  = uint8(0x00)
	P1DeriveKeyFromParent  = uint8(0x01)
	P1DeriveKeyFromCurrent = uint8(0x10)
	statusP1WalletStatus   = uint8(0x00)
	statusP1Path           = uint8(0x01)
	signP1PrecomputedHash  = uint8(0x01)
	signP2OnlyBlock        = uint8(0x81)
	exportP1Any            = uint8(0x00)
	exportP2Pubkey         = uint8(0x01)
)

// Minimum time to wait between self derivation attempts, even it the user is
// requesting accounts like crazy.
// ユーザーが狂ったようにアカウントを要求している場合でも、自己導出の試行の間に待機する最小時間。
const selfDeriveThrottling = time.Second

// Wallet represents a smartcard wallet instance.
// ウォレットはスマートカードウォレットインスタンスを表します。
type Wallet struct {
	Hub       *Hub   // このウォレットをインスタンス化したハブへのハンドル。// A handle to the Hub that instantiated this wallet.
	PublicKey []byte // ウォレットの公開鍵（署名ではなく、通信と識別に使用されます！）// The wallet's public key (used for communication and identification, not signing!)

	lock    sync.Mutex // 構造体フィールドへのアクセスとカードとの通信をゲートするロック // Lock that gates access to struct fields and communication with the card
	card    *pcsc.Card // ウォレットのスマートカードインターフェイスへのハンドル。// A handle to the smartcard interface for the wallet.
	session *Session   // カードとの安全な通信セッション// The secure communication session with the card
	log     log.Logger // ベースにIDをタグ付けするコンテキストロガー // Contextual logger to tag the base with its id

	deriveNextPaths []accounts.DerivationPath // アカウントの自動検出のための次の派生パス（複数のベースがサポートされています） // Next derivation paths for account auto-discovery (multiple bases supported)
	deriveNextAddrs []common.Address          // 自動検出用の次の派生アカウントアドレス（複数の拠点がサポートされています） // Next derived account addresses for auto-discovery (multiple bases supported)
	deriveChain     ethereum.ChainStateReader // 使用済みアカウントを検出するためのブロックチェーン状態リーダー // Blockchain state reader to discover used account with
	deriveReq       chan chan struct{}        // 自己派生を要求するチャネル     // Channel to request a self-derivation on
	deriveQuit      chan chan error           // セルフデリバを終了するチャネル // Channel to terminate the self-deriver with
}

// NewWallet constructs and returns a new Wallet instance.
// NewWalletは、新しいWalletインスタンスを作成して返します。
func NewWallet(hub *Hub, card *pcsc.Card) *Wallet {
	wallet := &Wallet{
		Hub:  hub,
		card: card,
	}
	return wallet
}

// transmit sends an APDU to the smartcard and receives and decodes the response.
// It automatically handles requests by the card to fetch the return data separately,
// and returns an error if the response status code is not success.
// 送信はAPDUをスマートカードに送信し、応答を受信して​​デコードします。
// カードによる要求を自動的に処理して、戻りデータを個別にフェッチし、応答ステータスコードが成功しなかった場合はエラーを返します。
func transmit(card *pcsc.Card, command *commandAPDU) (*responseAPDU, error) {
	data, err := command.serialize()
	if err != nil {
		return nil, err
	}

	responseData, _, err := card.Transmit(data)
	if err != nil {
		return nil, err
	}

	response := new(responseAPDU)
	if err = response.deserialize(responseData); err != nil {
		return nil, err
	}

	// Are we being asked to fetch the response separately?
	// 応答を個別に取得するように求められていますか？
	if response.Sw1 == sw1GetResponse && (command.Cla != claISO7816 || command.Ins != insGetResponse) {
		return transmit(card, &commandAPDU{
			Cla:  claISO7816,
			Ins:  insGetResponse,
			P1:   0,
			P2:   0,
			Data: nil,
			Le:   response.Sw2,
		})
	}

	if response.Sw1 != sw1Ok {
		return nil, fmt.Errorf("unexpected insecure response status Cla=0x%x, Ins=0x%x, Sw=0x%x%x", command.Cla, command.Ins, response.Sw1, response.Sw2)
	}

	return response, nil
}

// applicationInfo encodes information about the smartcard application - its
// instance UID and public key.
// applicationInfoは、スマートカードアプリケーションに関する情報（インスタンスUIDと公開鍵）をエンコードします。
type applicationInfo struct {
	InstanceUID []byte `asn1:"tag:15"`
	PublicKey   []byte `asn1:"tag:0"`
}

// connect connects to the wallet application and establishes a secure channel with it.
// must be called before any other interaction with the wallet.
// connectはウォレットアプリケーションに接続し、それを使用して安全なチャネルを確立します。
// ウォレットとのその他のやり取りの前に呼び出す必要があります。
func (w *Wallet) connect() error {
	w.lock.Lock()
	defer w.lock.Unlock()

	appinfo, err := w.doselect()
	if err != nil {
		return err
	}

	channel, err := NewSecureChannelSession(w.card, appinfo.PublicKey)
	if err != nil {
		return err
	}

	w.PublicKey = appinfo.PublicKey
	w.log = log.New("url", w.URL())
	w.session = &Session{
		Wallet:  w,
		Channel: channel,
	}
	return nil
}

// doselect is an internal (unlocked) function to send a SELECT APDU to the card.
// doselectは、SELECT APDUをカードに送信するための内部（ロック解除）関数です。
func (w *Wallet) doselect() (*applicationInfo, error) {
	response, err := transmit(w.card, &commandAPDU{
		Cla:  claISO7816,
		Ins:  insSelect,
		P1:   4,
		P2:   0,
		Data: appletAID,
	})
	if err != nil {
		return nil, err
	}

	appinfo := new(applicationInfo)
	if _, err := asn1.UnmarshalWithParams(response.Data, appinfo, "tag:4"); err != nil {
		return nil, err
	}
	return appinfo, nil
}

// ping checks the card's status and returns an error if unsuccessful.
// pingはカードのステータスをチェックし、失敗した場合はエラーを返します。
func (w *Wallet) ping() error {
	w.lock.Lock()
	defer w.lock.Unlock()

	// We can't ping if not paired
	// ペアになっていないとpingできません
	if !w.session.paired() {
		return nil
	}
	if _, err := w.session.walletStatus(); err != nil {
		return err
	}
	return nil
}

// release releases any resources held by an open wallet instance.
// リリースは、開いているウォレットインスタンスによって保持されているすべてのリソースを解放します。
func (w *Wallet) release() error {
	if w.session != nil {
		return w.session.release()
	}
	return nil
}

// pair is an internal (unlocked) function for establishing a new pairing
// with the wallet.
// ペアは、ウォレットとの新しいペアリングを確立するための内部（ロック解除）関数です。
func (w *Wallet) pair(puk []byte) error {
	if w.session.paired() {
		return fmt.Errorf("wallet already paired")
	}
	pairing, err := w.session.pair(puk)
	if err != nil {
		return err
	}
	if err = w.Hub.setPairing(w, &pairing); err != nil {
		return err
	}
	return w.session.authenticate(pairing)
}

// Unpair deletes an existing wallet pairing.
// ペアリングを解除すると、既存のウォレットのペアリングが削除されます。
func (w *Wallet) Unpair(pin []byte) error {
	w.lock.Lock()
	defer w.lock.Unlock()

	if !w.session.paired() {
		return fmt.Errorf("wallet %x not paired", w.PublicKey)
	}
	if err := w.session.verifyPin(pin); err != nil {
		return fmt.Errorf("failed to verify pin: %s", err)
	}
	if err := w.session.unpair(); err != nil {
		return fmt.Errorf("failed to unpair: %s", err)
	}
	if err := w.Hub.setPairing(w, nil); err != nil {
		return err
	}
	return nil
}

// URL retrieves the canonical path under which this wallet is reachable. It is
// user by upper layers to define a sorting order over all wallets from multiple
// backends.
// URLは、このウォレットに到達できる正規のパスを取得します。
// 複数のバックエンドからのすべてのウォレットの並べ替え順序を定義するのは、上位層ごとのユーザーです。
func (w *Wallet) URL() accounts.URL {
	return accounts.URL{
		Scheme: w.Hub.scheme,
		Path:   fmt.Sprintf("%x", w.PublicKey[1:5]), //バイト＃0は一意ではありません; 1：5は<< 64Kカードをカバーし、<< 4Mの場合は1：9にバンプします// Byte #0 isn't unique; 1:5 covers << 64K cards, bump to 1:9 for << 4M
	}
}

// Status returns a textual status to aid the user in the current state of the
// wallet. It also returns an error indicating any failure the wallet might have
// encountered.
// Statusは、ウォレットの現在の状態でユーザーを支援するためのテキストステータスを返します。
// また、ウォレットで発生した可能性のある障害を示すエラーも返します。
func (w *Wallet) Status() (string, error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	// If the card is not paired, we can only wait
	// カードがペアリングされていない場合、待つことしかできません
	if !w.session.paired() {
		return "Unpaired, waiting for pairing password", nil
	}
	// Yay, we have an encrypted session, retrieve the actual status
	// はい、暗号化されたセッションがあり、実際のステータスを取得します
	status, err := w.session.walletStatus()
	if err != nil {
		return fmt.Sprintf("Failed: %v", err), err
	}
	switch {
	case !w.session.verified && status.PinRetryCount == 0 && status.PukRetryCount == 0:
		return "Bricked, waiting for full wipe", nil
	case !w.session.verified && status.PinRetryCount == 0:
		return fmt.Sprintf("Blocked, waiting for PUK (%d attempts left) and new PIN", status.PukRetryCount), nil
	case !w.session.verified:
		return fmt.Sprintf("Locked, waiting for PIN (%d attempts left)", status.PinRetryCount), nil
	case !status.Initialized:
		return "Empty, waiting for initialization", nil
	default:
		return "Online", nil
	}
}

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
// （特にハードウェアウォレットを操作する場合に重要です）。
func (w *Wallet) Open(passphrase string) error {
	w.lock.Lock()
	defer w.lock.Unlock()

	// If the session is already open, bail out
	// セッションがすでに開いている場合は、ベイルアウトします
	if w.session.verified {
		return ErrAlreadyOpen
	}
	// If the smart card is not yet paired, attempt to do so either from a previous
	// pairing key or form the supplied PUK code.
	// スマートカードがまだペアリングされていない場合は、
	// 以前のペアリングキーからペアリングするか、提供されたPUKコードを作成してください。
	if !w.session.paired() {
		// If a previous pairing exists, only ever try to use that
		// 以前のペアリングが存在する場合は、それを使用しようとするだけです
		if pairing := w.Hub.pairing(w); pairing != nil {
			if err := w.session.authenticate(*pairing); err != nil {
				return fmt.Errorf("failed to authenticate card %x: %s", w.PublicKey[:4], err)
			}
			// Pairing still ok, fall through to PIN checks
			// ペアリングはまだ問題ありません、PINチェックにフォールスルーします
		} else {
			// If no passphrase was supplied, request the PUK from the user
			// パスフレーズが指定されていない場合は、ユーザーにPUKをリクエストします
			if passphrase == "" {
				return ErrPairingPasswordNeeded
			}
			// Attempt to pair the smart card with the user supplied PUK
			// スマートカードをユーザー提供のPUKとペアリングしようとします
			if err := w.pair([]byte(passphrase)); err != nil {
				return err
			}
			// Pairing succeeded, fall through to PIN checks. This will of course fail,
			// but we can't return ErrPINNeeded directly here because we don't know whether
			// a PIN check or a PIN reset is needed.
			// ペアリングが成功し、PINチェックにフォールスルーします。
			// もちろんこれは失敗しますが、 PINチェックまたはPINリセットが必要かどうかわからないため、
			// ここでErrPINNeededを直接返すことはできません。
			passphrase = ""
		}
	}
	// The smart card was successfully paired, retrieve its status to check whether
	// PIN verification or unblocking is needed.
	// スマートカードは正常にペアリングされました。
	// ステータスを取得して、PINの確認またはブロック解除が必要かどうかを確認します。
	status, err := w.session.walletStatus()
	if err != nil {
		return err
	}
	// Request the appropriate next authentication data, or use the one supplied
	// 適切な次の認証データを要求するか、提供されたものを使用します
	switch {
	case passphrase == "" && status.PinRetryCount > 0:
		return ErrPINNeeded
	case passphrase == "":
		return ErrPINUnblockNeeded
	case status.PinRetryCount > 0:
		if !regexp.MustCompile(`^[0-9]{6,}$`).MatchString(passphrase) {
			w.log.Error("PIN needs to be at least 6 digits")
			return ErrPINNeeded
		}
		if err := w.session.verifyPin([]byte(passphrase)); err != nil {
			return err
		}
	default:
		if !regexp.MustCompile(`^[0-9]{12,}$`).MatchString(passphrase) {
			w.log.Error("PUK needs to be at least 12 digits")
			return ErrPINUnblockNeeded
		}
		if err := w.session.unblockPin([]byte(passphrase)); err != nil {
			return err
		}
	}
	// Smart card paired and unlocked, initialize and register
	// スマートカードのペアリングとロック解除、初期化と登録
	w.deriveReq = make(chan chan struct{})
	w.deriveQuit = make(chan chan error)

	go w.selfDerive()

	// Notify anyone listening for wallet events that a new device is accessible
	// ウォレットイベントをリッスンしている人に、新しいデバイスにアクセスできることを通知します
	go w.Hub.updateFeed.Send(accounts.WalletEvent{Wallet: w, Kind: accounts.WalletOpened})

	return nil
}

// Close stops and closes the wallet, freeing any resources.
func (w *Wallet) Close() error {
	// Ensure the wallet was opened
	w.lock.Lock()
	dQuit := w.deriveQuit
	w.lock.Unlock()

	// Terminate the self-derivations
	var derr error
	if dQuit != nil {
		errc := make(chan error)
		dQuit <- errc
		derr = <-errc // Save for later, we *must* close the USB
	}
	// Terminate the device connection
	w.lock.Lock()
	defer w.lock.Unlock()

	w.deriveQuit = nil
	w.deriveReq = nil

	if err := w.release(); err != nil {
		return err
	}
	return derr
}

// selfDerive is an account derivation loop that upon request attempts to find
// new non-zero accounts.
// selfDeriveはアカウント派生ループであり、要求に応じてゼロ以外の新しいアカウントを見つけようとします。
func (w *Wallet) selfDerive() {
	w.log.Debug("Smart card wallet self-derivation started")
	defer w.log.Debug("Smart card wallet self-derivation stopped")

	// Execute self-derivations until termination or error
	// 終了またはエラーになるまで自己導出を実行します
	var (
		reqc chan struct{}
		errc chan error
		err  error
	)
	for errc == nil && err == nil {
		// Wait until either derivation or termination is requested
		// 派生または終了のいずれかが要求されるまで待機します
		select {
		case errc = <-w.deriveQuit:
			// Termination requested
			// 終了が要求されました
			continue
		case reqc = <-w.deriveReq:
			// Account discovery requested
			// アカウントの検出が要求されました
		}
		// Derivation needs a chain and device access, skip if either unavailable
		// 派生にはチェーンとデバイスへのアクセスが必要です。どちらかが利用できない場合は、スキップしてください
		w.lock.Lock()
		if w.session == nil || w.deriveChain == nil {
			w.lock.Unlock()
			reqc <- struct{}{}
			continue
		}
		pairing := w.Hub.pairing(w)

		// Device lock obtained, derive the next batch of accounts
		// デバイスロックを取得し、アカウントの次のバッチを取得します
		var (
			paths   []accounts.DerivationPath
			nextAcc accounts.Account

			nextPaths = append([]accounts.DerivationPath{}, w.deriveNextPaths...)
			nextAddrs = append([]common.Address{}, w.deriveNextAddrs...)

			context = context.Background()
		)
		for i := 0; i < len(nextAddrs); i++ {
			for empty := false; !empty; {
				// Retrieve the next derived Ethereum account
				// 次に派生したイーサリアムアカウントを取得します
				if nextAddrs[i] == (common.Address{}) {
					if nextAcc, err = w.session.derive(nextPaths[i]); err != nil {
						w.log.Warn("Smartcard wallet account derivation failed", "err", err)
						break
					}
					nextAddrs[i] = nextAcc.Address
				}
				// Check the account's status against the current chain state
				// アカウントのステータスを現在のチェーン状態と照合します
				var (
					balance *big.Int
					nonce   uint64
				)
				balance, err = w.deriveChain.BalanceAt(context, nextAddrs[i], nil)
				if err != nil {
					w.log.Warn("Smartcard wallet balance retrieval failed", "err", err)
					break
				}
				nonce, err = w.deriveChain.NonceAt(context, nextAddrs[i], nil)
				if err != nil {
					w.log.Warn("Smartcard wallet nonce retrieval failed", "err", err)
					break
				}
				// If the next account is empty, stop self-derivation, but add for the last base path
				// 次のアカウントが空の場合、自己派生を停止しますが、最後のベースパスを追加します
				if balance.Sign() == 0 && nonce == 0 {
					empty = true
					if i < len(nextAddrs)-1 {
						break
					}
				}
				// We've just self-derived a new account, start tracking it locally
				// 新しいアカウントを自己作成し、ローカルで追跡を開始しました
				path := make(accounts.DerivationPath, len(nextPaths[i]))
				copy(path[:], nextPaths[i][:])
				paths = append(paths, path)

				// Display a log message to the user for new (or previously empty accounts)
				// 新しい（または以前は空だった）アカウントのログメッセージをユーザーに表示します
				if _, known := pairing.Accounts[nextAddrs[i]]; !known || !empty || nextAddrs[i] != w.deriveNextAddrs[i] {
					w.log.Info("Smartcard wallet discovered new account", "address", nextAddrs[i], "path", path, "balance", balance, "nonce", nonce)
				}
				pairing.Accounts[nextAddrs[i]] = path

				// Fetch the next potential account
				// 次の潜在的なアカウントを取得します
				if !empty {
					nextAddrs[i] = common.Address{}
					nextPaths[i][len(nextPaths[i])-1]++
				}
			}
		}
		// If there are new accounts, write them out
		// 新しいアカウントがある場合は、それらを書き出します
		if len(paths) > 0 {
			err = w.Hub.setPairing(w, pairing)
		}
		// Shift the self-derivation forward
		// 自己派生を前方にシフトします
		w.deriveNextAddrs = nextAddrs
		w.deriveNextPaths = nextPaths

		// Self derivation complete, release device lock
		// 自己導出が完了し、デバイスロックを解除します
		w.lock.Unlock()

		// Notify the user of termination and loop after a bit of time (to avoid trashing)
		// ユーザーに終了を通知し、しばらくするとループします（ゴミ箱を避けるため）
		reqc <- struct{}{}
		if err == nil {
			select {
			case errc = <-w.deriveQuit:
				// Termination requested, abort
				// 終了が要求され、中止します
			case <-time.After(selfDeriveThrottling):
				// Waited enough, willing to self-derive again
				// 十分に待って、再び自己派生することをいとわない
			}
		}
	}
	// In case of error, wait for termination
	// エラーが発生した場合は、終了を待ちます
	if err != nil {
		w.log.Debug("Smartcard wallet self-derivation failed", "err", err)
		errc = <-w.deriveQuit
	}
	errc <- err
}

// Accounts retrieves the list of signing accounts the wallet is currently aware
// of. For hierarchical deterministic wallets, the list will not be exhaustive,
// rather only contain the accounts explicitly pinned during account derivation.
// アカウントは、ウォレットが現在認識している署名アカウントのリストを取得します。
// 階層的な決定論的ウォレットの場合、リストは網羅的ではなく、
// アカウントの派生中に明示的に固定されたアカウントのみが含まれます。
func (w *Wallet) Accounts() []accounts.Account {
	// Attempt self-derivation if it's running
	// 実行中の場合は自己導出を試みます
	reqc := make(chan struct{}, 1)
	select {
	case w.deriveReq <- reqc:
		// Self-derivation request accepted, wait for it
		// 自己派生リクエストが受け入れられました。それを待ちます
		<-reqc
	default:
		// Self-derivation offline, throttled or busy, skip
		// 自己派生オフライン、スロットルまたはビジー、スキップ
	}

	w.lock.Lock()
	defer w.lock.Unlock()

	if pairing := w.Hub.pairing(w); pairing != nil {
		ret := make([]accounts.Account, 0, len(pairing.Accounts))
		for address, path := range pairing.Accounts {
			ret = append(ret, w.makeAccount(address, path))
		}
		sort.Sort(accounts.AccountsByURL(ret))
		return ret
	}
	return nil
}

func (w *Wallet) makeAccount(address common.Address, path accounts.DerivationPath) accounts.Account {
	return accounts.Account{
		Address: address,
		URL: accounts.URL{
			Scheme: w.Hub.scheme,
			Path:   fmt.Sprintf("%x/%s", w.PublicKey[1:3], path.String()),
		},
	}
}

// Contains returns whether an account is part of this particular wallet or not.
// アカウントがこの特定のウォレットの一部であるかどうかを返します。
func (w *Wallet) Contains(account accounts.Account) bool {
	if pairing := w.Hub.pairing(w); pairing != nil {
		_, ok := pairing.Accounts[account.Address]
		return ok
	}
	return false
}

// Initialize installs a keypair generated from the provided key into the wallet.
// Initializeは、提供されたキーから生成されたキーペアをウォレットにインストールします。
func (w *Wallet) Initialize(seed []byte) error {
	go w.selfDerive()
	// DO NOT lock at this stage, as the initialize
	// function relies on Status()
	// 初期化関数はStatus（）に依存しているため、この段階ではロックしないでください
	return w.session.initialize(seed)
}

// Derive attempts to explicitly derive a hierarchical deterministic account at
// the specified derivation path. If requested, the derived account will be added
// to the wallet's tracked account list.
// 派生は、指定された派生パスで階層的な決定論的アカウントを明示的に派生させようとします。
// 要求された場合、派生アカウントはウォレットの追跡アカウントリストに追加されます。
func (w *Wallet) Derive(path accounts.DerivationPath, pin bool) (accounts.Account, error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	account, err := w.session.derive(path)
	if err != nil {
		return accounts.Account{}, err
	}

	if pin {
		pairing := w.Hub.pairing(w)
		pairing.Accounts[account.Address] = path
		if err := w.Hub.setPairing(w, pairing); err != nil {
			return accounts.Account{}, err
		}
	}

	return account, nil
}

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
// 一部のハードウェアウォレットは、進化を通じて派生パスを切り替えたため、このメソッドは、
// 古いユーザーアカウントを検出するための複数のベースの提供もサポートします。
// 最後のベースのみが次の空のアカウントを導出するために使用されます。
//
// nilチェーン状態リーダーでSelfDeriveを呼び出すことにより、自動アカウント検出を無効にできます。
func (w *Wallet) SelfDerive(bases []accounts.DerivationPath, chain ethereum.ChainStateReader) {
	w.lock.Lock()
	defer w.lock.Unlock()

	w.deriveNextPaths = make([]accounts.DerivationPath, len(bases))
	for i, base := range bases {
		w.deriveNextPaths[i] = make(accounts.DerivationPath, len(base))
		copy(w.deriveNextPaths[i][:], base[:])
	}
	w.deriveNextAddrs = make([]common.Address, len(bases))
	w.deriveChain = chain
}

// SignData requests the wallet to sign the hash of the given data.
//
// It looks up the account specified either solely via its address contained within,
// or optionally with the aid of any location metadata from the embedded URL field.
//
// If the wallet requires additional authentication to sign the request (e.g.
// a password to decrypt the account, or a PIN code o verify the transaction),
// an AuthNeededError instance will be returned, containing infos for the user
// about which fields or actions are needed. The user may retry by providing
// the needed details via SignDataWithPassphrase, or by other means (e.g. unlock
// the account in a keystore).
// SignDataは、指定されたデータのハッシュに署名するようウォレットに要求します。
//
// 内に含まれるアドレスのみを介して、またはオプションで埋め込みURLフィールドの場所メタデータを使用して指定されたアカウントを検索します。
//
// ウォレットがリクエストに署名するために追加の認証を必要とする場合（例：
// アカウントを復号化するためのパスワード、またはトランザクションを確認するためのPINコード）、
// AuthNeededErrorインスタンスが返され、必要なフィールドまたはアクションに関するユーザーの情報が含まれます。
// ユーザーは、SignDataWithPassphraseを介して、または他の手段（たとえば、キーストアでアカウントのロックを解除する）によって、
// 必要な詳細を提供することによって再試行できます。
func (w *Wallet) SignData(account accounts.Account, mimeType string, data []byte) ([]byte, error) {
	return w.signHash(account, crypto.Keccak256(data))
}

func (w *Wallet) signHash(account accounts.Account, hash []byte) ([]byte, error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	path, err := w.findAccountPath(account)
	if err != nil {
		return nil, err
	}

	return w.session.sign(path, hash)
}

// SignTx requests the wallet to sign the given transaction.
//
// It looks up the account specified either solely via its address contained within,
// or optionally with the aid of any location metadata from the embedded URL field.
//
// If the wallet requires additional authentication to sign the request (e.g.
// a password to decrypt the account, or a PIN code o verify the transaction),
// an AuthNeededError instance will be returned, containing infos for the user
// about which fields or actions are needed. The user may retry by providing
// the needed details via SignTxWithPassphrase, or by other means (e.g. unlock
// the account in a keystore).
// SignTxは、指定されたトランザクションに署名するようウォレットに要求します。
//
// 内に含まれるアドレスのみを介して、またはオプションで埋め込みURLフィールドの場所メタデータを使用して指定されたアカウントを検索します。
//
// ウォレットがリクエストに署名するために追加の認証を必要とする場合
// （たとえば、アカウントを復号化するためのパスワード、またはトランザクションを確認するためのPINコード）、
// AuthNeededErrorインスタンスが返され、必要なフィールドまたはアクションに関するユーザーの情報が含まれます。
// ユーザーは、SignTxWithPassphraseを介して、または他の手段（たとえば、キーストアでアカウントのロックを解除する）によって、
// 必要な詳細を提供することによって再試行できます。
func (w *Wallet) SignTx(account accounts.Account, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	signer := types.LatestSignerForChainID(chainID)
	hash := signer.Hash(tx)
	sig, err := w.signHash(account, hash[:])
	if err != nil {
		return nil, err
	}
	return tx.WithSignature(signer, sig)
}

// SignDataWithPassphrase requests the wallet to sign the given hash with the
// given passphrase as extra authentication information.
//
// It looks up the account specified either solely via its address contained within,
// or optionally with the aid of any location metadata from the embedded URL field.
// SignDataWithPassphraseは、追加の認証情報として、
// 指定されたパスフレーズを使用して指定されたハッシュに署名するようウォレットに要求します。
//
// 内に含まれるアドレスのみを介して、またはオプションで埋め込みURLフィールドの場所メタデータを使用して指定されたアカウントを検索します。
func (w *Wallet) SignDataWithPassphrase(account accounts.Account, passphrase, mimeType string, data []byte) ([]byte, error) {
	return w.signHashWithPassphrase(account, passphrase, crypto.Keccak256(data))
}

func (w *Wallet) signHashWithPassphrase(account accounts.Account, passphrase string, hash []byte) ([]byte, error) {
	if !w.session.verified {
		if err := w.Open(passphrase); err != nil {
			return nil, err
		}
	}

	return w.signHash(account, hash)
}

// SignText requests the wallet to sign the hash of a given piece of data, prefixed
// by the Ethereum prefix scheme
// It looks up the account specified either solely via its address contained within,
// or optionally with the aid of any location metadata from the embedded URL field.
//
// If the wallet requires additional authentication to sign the request (e.g.
// a password to decrypt the account, or a PIN code o verify the transaction),
// an AuthNeededError instance will be returned, containing infos for the user
// about which fields or actions are needed. The user may retry by providing
// the needed details via SignHashWithPassphrase, or by other means (e.g. unlock
// the account in a keystore).
// SignTextは、Ethereumプレフィックススキームでプレフィックスが付けられた、
// 特定のデータのハッシュに署名するようウォレットに要求します。
// SignTextは、含まれているアドレスのみを介して、またはオプションで埋め込みURLのロケーションメタデータを使用して、
// 指定されたアカウントを検索します。分野。
//
// ウォレットがリクエストに署名するために追加の認証を必要とする場合（たとえば、アカウントを復号化するためのパスワード、
// またはトランザクションを確認するためのPINコード）、AuthNeededErrorインスタンスが返され、
// 必要なフィールドまたはアクションに関するユーザーの情報が含まれます。
// ユーザーは、SignHashWithPassphraseを介して、または他の手段
// （たとえば、キーストアでアカウントのロックを解除する）によって、必要な詳細を提供することによって再試行できます。
func (w *Wallet) SignText(account accounts.Account, text []byte) ([]byte, error) {
	return w.signHash(account, accounts.TextHash(text))
}

// SignTextWithPassphrase implements accounts.Wallet, attempting to sign the
// given hash with the given account using passphrase as extra authentication
// SignTextWithPassphraseはaccounts.Walletを実装し、追加の認証としてパスフレーズを使用して、
// 指定されたアカウントで指定されたハッシュに署名しようとします
func (w *Wallet) SignTextWithPassphrase(account accounts.Account, passphrase string, text []byte) ([]byte, error) {
	return w.signHashWithPassphrase(account, passphrase, crypto.Keccak256(accounts.TextHash(text)))
}

// SignTxWithPassphrase requests the wallet to sign the given transaction, with the
// given passphrase as extra authentication information.
//
// It looks up the account specified either solely via its address contained within,
// or optionally with the aid of any location metadata from the embedded URL field.
// SignTxWithPassphraseは、追加の認証情報として指定されたパスフレーズを使用して、
// 指定されたトランザクションに署名するようウォレットに要求します。
//
// 内に含まれるアドレスのみを介して、またはオプションで埋め込みURLフィールドの場所メタデータを使用して指定されたアカウントを検索します。
func (w *Wallet) SignTxWithPassphrase(account accounts.Account, passphrase string, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	if !w.session.verified {
		if err := w.Open(passphrase); err != nil {
			return nil, err
		}
	}
	return w.SignTx(account, tx, chainID)
}

// findAccountPath returns the derivation path for the provided account.
// It first checks for the address in the list of pinned accounts, and if it is
// not found, attempts to parse the derivation path from the account's URL.
// findAccountPathは、指定されたアカウントの派生パスを返します。
// 最初に、固定されたアカウントのリストでアドレスをチェックし、
// アドレスが見つからない場合は、アカウントのURLから派生パスを解析しようとします。
func (w *Wallet) findAccountPath(account accounts.Account) (accounts.DerivationPath, error) {
	pairing := w.Hub.pairing(w)
	if path, ok := pairing.Accounts[account.Address]; ok {
		return path, nil
	}

	// Look for the path in the URL
	// URLでパスを探します
	if account.URL.Scheme != w.Hub.scheme {
		return nil, fmt.Errorf("scheme %s does not match wallet scheme %s", account.URL.Scheme, w.Hub.scheme)
	}

	parts := strings.SplitN(account.URL.Path, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid URL format: %s", account.URL)
	}

	if parts[0] != fmt.Sprintf("%x", w.PublicKey[1:3]) {
		return nil, fmt.Errorf("URL %s is not for this wallet", account.URL)
	}

	return accounts.ParseDerivationPath(parts[1])
}

// Session represents a secured communication session with the wallet.
// セッションは、ウォレットとの安全な通信セッションを表します。
type Session struct {
	Wallet   *Wallet               // セッションを開いたウォレットへのハンドル  // A handle to the wallet that opened the session
	Channel  *SecureChannelSession // 暗号化されたメッセージの安全なチャネル    // A secure channel for encrypted messages
	verified bool                  // このセッションでピンが確認されたかどうか。// Whether the pin has been verified in this session.
}

// pair establishes a new pairing over this channel, using the provided secret.
// ペアは、提供されたシークレットを使用して、このチャネルを介して新しいペアリングを確立します。
func (s *Session) pair(secret []byte) (smartcardPairing, error) {
	err := s.Channel.Pair(secret)
	if err != nil {
		return smartcardPairing{}, err
	}

	return smartcardPairing{
		PublicKey:    s.Wallet.PublicKey,
		PairingIndex: s.Channel.PairingIndex,
		PairingKey:   s.Channel.PairingKey,
		Accounts:     make(map[common.Address]accounts.DerivationPath),
	}, nil
}

// unpair deletes an existing pairing.
// ペアリングを解除すると、既存のペアリングが削除されます。
func (s *Session) unpair() error {
	if !s.verified {
		return fmt.Errorf("unpair requires that the PIN be verified")
	}
	return s.Channel.Unpair()
}

// verifyPin unlocks a wallet with the provided pin.
// verifyPinは、提供されたピンでウォレットのロックを解除します。
func (s *Session) verifyPin(pin []byte) error {
	if _, err := s.Channel.transmitEncrypted(claSCWallet, insVerifyPin, 0, 0, pin); err != nil {
		return err
	}
	s.verified = true
	return nil
}

// unblockPin unblocks a wallet with the provided puk and resets the pin to the
// new one specified.
// unblockPinは、提供されたpukでウォレットのブロックを解除し、ピンを指定された新しいものにリセットします。
func (s *Session) unblockPin(pukpin []byte) error {
	if _, err := s.Channel.transmitEncrypted(claSCWallet, insUnblockPin, 0, 0, pukpin); err != nil {
		return err
	}
	s.verified = true
	return nil
}

// release releases resources associated with the channel.
// リリースはチャネルに関連付けられたリソースを解放します。
func (s *Session) release() error {
	return s.Wallet.card.Disconnect(pcsc.LeaveCard)
}

// paired returns true if a valid pairing exists.
// 有効なペアリングが存在する場合、pairedはtrueを返します。
func (s *Session) paired() bool {
	return s.Channel.PairingKey != nil
}

// authenticate uses an existing pairing to establish a secure channel.
// 認証は、既存のペアリングを使用してセキュリティで保護されたチャネルを確立します。
func (s *Session) authenticate(pairing smartcardPairing) error {
	if !bytes.Equal(s.Wallet.PublicKey, pairing.PublicKey) {
		return fmt.Errorf("cannot pair using another wallet's pairing; %x != %x", s.Wallet.PublicKey, pairing.PublicKey)
	}
	s.Channel.PairingKey = pairing.PairingKey
	s.Channel.PairingIndex = pairing.PairingIndex
	return s.Channel.Open()
}

// walletStatus describes a smartcard wallet's status information.
// walletStatusは、スマートカードウォレットのステータス情報を記述します。
type walletStatus struct {
	PinRetryCount int  // 残りのPIN再試行回数  // Number of remaining PIN retries
	PukRetryCount int  // 残りのPUK再試行の数  // Number of remaining PUK retries
	Initialized   bool // Whether the card has been initialized with a private key
}

// walletStatus fetches the wallet's status from the card.
// walletStatusは、カードからウォレットのステータスを取得します。
func (s *Session) walletStatus() (*walletStatus, error) {
	response, err := s.Channel.transmitEncrypted(claSCWallet, insStatus, statusP1WalletStatus, 0, nil)
	if err != nil {
		return nil, err
	}

	status := new(walletStatus)
	if _, err := asn1.UnmarshalWithParams(response.Data, status, "tag:3"); err != nil {
		return nil, err
	}
	return status, nil
}

// derivationPath fetches the wallet's current derivation path from the card.
//lint:ignore U1000 needs to be added to the console interface
// derivationPathは、カードからウォレットの現在の派生パスをフェッチします。
// lint：ignoreU1000をコンソールインターフェイスに追加する必要があります
func (s *Session) derivationPath() (accounts.DerivationPath, error) {
	response, err := s.Channel.transmitEncrypted(claSCWallet, insStatus, statusP1Path, 0, nil)
	if err != nil {
		return nil, err
	}
	buf := bytes.NewReader(response.Data)
	path := make(accounts.DerivationPath, len(response.Data)/4)
	return path, binary.Read(buf, binary.BigEndian, &path)
}

// initializeData contains data needed to initialize the smartcard wallet.
// initializeDataには、スマートカードウォレットを初期化するために必要なデータが含まれています。
type initializeData struct {
	PublicKey  []byte `asn1:"tag:0"`
	PrivateKey []byte `asn1:"tag:1"`
	ChainCode  []byte `asn1:"tag:2"`
}

// initialize initializes the card with new key data.
// initializeは、新しいキーデータでカードを初期化します。
func (s *Session) initialize(seed []byte) error {
	// Check that the wallet isn't currently initialized,
	// otherwise the key would be overwritten.
	// ウォレットが現在初期化されていないことを確認します。初期化されていない場合、キーが上書きされます。
	status, err := s.Wallet.Status()
	if err != nil {
		return err
	}
	if status == "Online" {
		return fmt.Errorf("card is already initialized, cowardly refusing to proceed") // カードはすでに初期化されており、臆病に続行を拒否しています
	}

	s.Wallet.lock.Lock()
	defer s.Wallet.lock.Unlock()

	// HMAC the seed to produce the private key and chain code
	// 秘密鍵とチェーンコードを生成するシードをHMACします
	mac := hmac.New(sha512.New, []byte("Bitcoin seed"))
	mac.Write(seed)
	seed = mac.Sum(nil)

	key, err := crypto.ToECDSA(seed[:32])
	if err != nil {
		return err
	}

	id := initializeData{}
	id.PublicKey = crypto.FromECDSAPub(&key.PublicKey)
	id.PrivateKey = seed[:32]
	id.ChainCode = seed[32:]
	data, err := asn1.Marshal(id)
	if err != nil {
		return err
	}

	// Nasty hack to force the top-level struct tag to be context-specific
	// 最上位の構造体タグをコンテキスト固有にするための厄介なハック
	data[0] = 0xA1

	_, err = s.Channel.transmitEncrypted(claSCWallet, insLoadKey, 0x02, 0, data)
	return err
}

// derive derives a new HD key path on the card.
// 派生は、カード上の新しいHDキーパスを派生させます。
func (s *Session) derive(path accounts.DerivationPath) (accounts.Account, error) {
	startingPoint, path, err := derivationpath.Decode(path.String())
	if err != nil {
		return accounts.Account{}, err
	}

	var p1 uint8
	switch startingPoint {
	case derivationpath.StartingPointMaster:
		p1 = P1DeriveKeyFromMaster
	case derivationpath.StartingPointParent:
		p1 = P1DeriveKeyFromParent
	case derivationpath.StartingPointCurrent:
		p1 = P1DeriveKeyFromCurrent
	default:
		return accounts.Account{}, fmt.Errorf("invalid startingPoint %d", startingPoint)
	}

	data := new(bytes.Buffer)
	for _, segment := range path {
		if err := binary.Write(data, binary.BigEndian, segment); err != nil {
			return accounts.Account{}, err
		}
	}

	_, err = s.Channel.transmitEncrypted(claSCWallet, insDeriveKey, p1, 0, data.Bytes())
	if err != nil {
		return accounts.Account{}, err
	}

	response, err := s.Channel.transmitEncrypted(claSCWallet, insSign, 0, 0, DerivationSignatureHash[:])
	if err != nil {
		return accounts.Account{}, err
	}

	sigdata := new(signatureData)
	if _, err := asn1.UnmarshalWithParams(response.Data, sigdata, "tag:0"); err != nil {
		return accounts.Account{}, err
	}
	rbytes, sbytes := sigdata.Signature.R.Bytes(), sigdata.Signature.S.Bytes()
	sig := make([]byte, 65)
	copy(sig[32-len(rbytes):32], rbytes)
	copy(sig[64-len(sbytes):64], sbytes)

	if err := confirmPublicKey(sig, sigdata.PublicKey); err != nil {
		return accounts.Account{}, err
	}
	pub, err := crypto.UnmarshalPubkey(sigdata.PublicKey)
	if err != nil {
		return accounts.Account{}, err
	}
	return s.Wallet.makeAccount(crypto.PubkeyToAddress(*pub), path), nil
}

// keyExport contains information on an exported keypair.
// lint:ignore U1000 needs to be added to the console interface
// keyExportには、エクスポートされたキーペアに関する情報が含まれています。
// lint：ignoreU1000をコンソールインターフェイスに追加する必要があります
type keyExport struct {
	PublicKey  []byte `asn1:"tag:0"`
	PrivateKey []byte `asn1:"tag:1,optional"`
}

// publicKey returns the public key for the current derivation path.
//lint:ignore U1000 needs to be added to the console interface
// publicKeyは、現在の派生パスの公開鍵を返します。
// lint：ignoreU1000をコンソールインターフェイスに追加する必要があります
func (s *Session) publicKey() ([]byte, error) {
	response, err := s.Channel.transmitEncrypted(claSCWallet, insExportKey, exportP1Any, exportP2Pubkey, nil)
	if err != nil {
		return nil, err
	}
	keys := new(keyExport)
	if _, err := asn1.UnmarshalWithParams(response.Data, keys, "tag:1"); err != nil {
		return nil, err
	}
	return keys.PublicKey, nil
}

// signatureData contains information on a signature - the signature itself and
// the corresponding public key.
// signatureDataには署名に関する情報が含まれています-署名自体と対応する公開鍵。
type signatureData struct {
	PublicKey []byte `asn1:"tag:0"`
	Signature struct {
		R *big.Int
		S *big.Int
	}
}

// sign asks the card to sign a message, and returns a valid signature after
// recovering the v value.
// signはカードにメッセージに署名するように要求し、v値を回復した後に有効な署名を返します。
func (s *Session) sign(path accounts.DerivationPath, hash []byte) ([]byte, error) {
	startTime := time.Now()
	_, err := s.derive(path)
	if err != nil {
		return nil, err
	}
	deriveTime := time.Now()

	response, err := s.Channel.transmitEncrypted(claSCWallet, insSign, signP1PrecomputedHash, signP2OnlyBlock, hash)
	if err != nil {
		return nil, err
	}
	sigdata := new(signatureData)
	if _, err := asn1.UnmarshalWithParams(response.Data, sigdata, "tag:0"); err != nil {
		return nil, err
	}
	// Serialize the signature
	// 署名をシリアル化します
	rbytes, sbytes := sigdata.Signature.R.Bytes(), sigdata.Signature.S.Bytes()
	sig := make([]byte, 65)
	copy(sig[32-len(rbytes):32], rbytes)
	copy(sig[64-len(sbytes):64], sbytes)

	// Recover the V value.
	// V値を回復します。
	sig, err = makeRecoverableSignature(hash, sig, sigdata.PublicKey)
	if err != nil {
		return nil, err
	}
	log.Debug("Signed using smartcard", "deriveTime", deriveTime.Sub(startTime), "signingTime", time.Since(deriveTime))

	return sig, nil
}

// confirmPublicKey confirms that the given signature belongs to the specified key.
// confirmPublicKeyは、指定された署名が指定されたキーに属していることを確認します。
func confirmPublicKey(sig, pubkey []byte) error {
	_, err := makeRecoverableSignature(DerivationSignatureHash[:], sig, pubkey)
	return err
}

// makeRecoverableSignature uses a signature and an expected public key to
// recover the v value and produce a recoverable signature.
// makeRecoverableSignatureは、署名と予想される公開鍵を使用してv値を回復し、回復可能な署名を生成します。
func makeRecoverableSignature(hash, sig, expectedPubkey []byte) ([]byte, error) {
	var libraryError error
	for v := 0; v < 2; v++ {
		sig[64] = byte(v)
		if pubkey, err := crypto.Ecrecover(hash, sig); err == nil {
			if bytes.Equal(pubkey, expectedPubkey) {
				return sig, nil
			}
		} else {
			libraryError = err
		}
	}
	if libraryError != nil {
		return nil, libraryError
	}
	return nil, ErrPubkeyMismatch
}
