// Copyright 2016 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"io/ioutil"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"gopkg.in/urfave/cli.v1"
)

var (
	walletCommand = cli.Command{
		Name:      "wallet",
		Usage:     "Manage Ethereum presale wallets", // イーサリアムの先行販売ウォレットを管理する
		ArgsUsage: "",
		Category:  "ACCOUNT COMMANDS",
		Description: `
    geth wallet import /path/to/my/presale.wallet

will prompt for your password and imports your ether presale account.
It can be used non-interactively with the --password option taking a
passwordfile as argument containing the wallet password in plaintext.`,
		// パスワードの入力を求められ、etherpresaleアカウントがインポートされます。
		// --passwordオプションを使用して非対話的に使用できます。
		// プレーンテキストのウォレットパスワードを含む引数としてのpasswordfile。
		Subcommands: []cli.Command{
			{

				Name:      "import",
				Usage:     "Import Ethereum presale wallet", // イーサリアムの先行販売ウォレットをインポートする
				ArgsUsage: "<keyFile>",
				Action:    utils.MigrateFlags(importWallet),
				Category:  "ACCOUNT COMMANDS",
				Flags: []cli.Flag{
					utils.DataDirFlag,
					utils.KeyStoreDirFlag,
					utils.PasswordFileFlag,
					utils.LightKDFFlag,
				},
				Description: `
	geth wallet [options] /path/to/my/presale.wallet

will prompt for your password and imports your ether presale account.
It can be used non-interactively with the --password option taking a
passwordfile as argument containing the wallet password in plaintext.`,
				// パスワードの入力を求められ、etherpresaleアカウントがインポートされます。
				// --passwordオプションを使用して非対話的に使用できます。
				// プレーンテキストのウォレットパスワードを含む引数としてのpasswordfile。
			},
		},
	}

	accountCommand = cli.Command{
		Name:     "account",
		Usage:    "Manage accounts", // アカウントを管理する
		Category: "ACCOUNT COMMANDS",
		Description: `

Manage accounts, list all existing accounts, import a private key into a new
account, create a new account or update an existing account.

It supports interactive mode, when you are prompted for password as well as
non-interactive mode where passwords are supplied via a given password file.
Non-interactive mode is only meant for scripted use on test networks or known
safe environments.

Make sure you remember the password you gave when creating a new account (with
either new or import). Without it you are not able to unlock your account.

Note that exporting your key in unencrypted format is NOT supported.

Keys are stored under <DATADIR>/keystore.
It is safe to transfer the entire directory or the individual keys therein
between ethereum nodes by simply copying.

Make sure you backup your keys regularly.`,
		// アカウントを管理し、既存のすべてのアカウントを一覧表示し、秘密鍵を新しいアカウントにインポートします
		// アカウント、新しいアカウントを作成するか、既存のアカウントを更新します。

		// パスワードの入力を求められたときの対話型モードと、特定のパスワードファイルを介してパスワードが提供される非対話型モードをサポートします。
		// 非対話型モードは、テストネットワークまたは既知の安全な環境でのスクリプトによる使用のみを目的としています。

		// 新しいアカウントを作成するときに指定したパスワードを覚えておいてください（
		// 新規またはインポート）。それがないと、アカウントのロックを解除できません。

		// 暗号化されていない形式でのキーのエクスポートはサポートされていないことに注意してください。

		// キーは<DATADIR> / keystoreに保存されます。
		// ディレクトリ全体またはその中の個々のキーを安全に転送できます
		// 単にコピーすることにより、イーサリアムノード間で。

		// キーを定期的にバックアップしてください。
		Subcommands: []cli.Command{
			{
				Name:   "list",
				Usage:  "Print summary of existing accounts", // 既存のアカウントの概要を印刷する
				Action: utils.MigrateFlags(accountList),
				Flags: []cli.Flag{
					utils.DataDirFlag,
					utils.KeyStoreDirFlag,
				},
				Description: `
Print a short summary of all accounts`, // すべてのアカウントの簡単な要約を印刷する
			},
			{
				Name:   "new",
				Usage:  "Create a new account", // 新しいアカウントを作成する
				Action: utils.MigrateFlags(accountCreate),
				Flags: []cli.Flag{
					utils.DataDirFlag,
					utils.KeyStoreDirFlag,
					utils.PasswordFileFlag,
					utils.LightKDFFlag,
				},
				Description: `
    geth account new

Creates a new account and prints the address.

The account is saved in encrypted format, you are prompted for a password.

You must remember this password to unlock your account in the future.

For non-interactive use the password can be specified with the --password flag:

Note, this is meant to be used for testing only, it is a bad idea to save your
password to file or expose in any other way.
`,
				// gethアカウント新規
				// 新しいアカウントを作成し、アドレスを印刷します。
				// アカウントは暗号化された形式で保存され、パスワードの入力を求められます。
				// 今後アカウントのロックを解除するには、このパスワードを覚えておく必要があります。
				// 非対話型の使用の場合、パスワードは--passwordフラグで指定できます。
				// これはテストのみに使用することを目的としていることに注意してください。保存することはお勧めできません。
				// 他の方法でファイルまたは公開するためのパスワード。
			},
			{
				Name:      "update",
				Usage:     "Update an existing account", // 既存のアカウントを更新する
				Action:    utils.MigrateFlags(accountUpdate),
				ArgsUsage: "<address>",
				Flags: []cli.Flag{
					utils.DataDirFlag,
					utils.KeyStoreDirFlag,
					utils.LightKDFFlag,
				},
				Description: `
    geth account update <address>

Update an existing account.

The account is saved in the newest version in encrypted format, you are prompted
for a password to unlock the account and another to save the updated file.

This same command can therefore be used to migrate an account of a deprecated
format to the newest format or change the password for an account.

For non-interactive use the password can be specified with the --password flag:

    geth account update [options] <address>

Since only one password can be given, only format update can be performed,
changing your password is only possible interactively.
`,
				// gethアカウントの更新<アドレス>
				// 既存のアカウントを更新します。
				// アカウントは暗号化された形式で最新バージョンで保存され、プロンプトが表示されます
				// アカウントのロックを解除するためのパスワードと、更新されたファイルを保存するためのパスワード。

				// したがって、この同じコマンドを使用して、廃止されたアカウントを移行できます
				// 最新の形式にフォーマットするか、アカウントのパスワードを変更します。

				// 非対話型の使用の場合、パスワードは--passwordフラグで指定できます。
				//     gethアカウントの更新[オプション] <アドレス>
				// パスワードは1つしか指定できないため、フォーマットの更新のみを実行できます。
				// パスワードの変更はインタラクティブにのみ可能です。
			},
			{
				Name:   "import",
				Usage:  "Import a private key into a new account", // 秘密鍵を新しいアカウントにインポートします
				Action: utils.MigrateFlags(accountImport),
				Flags: []cli.Flag{
					utils.DataDirFlag,
					utils.KeyStoreDirFlag,
					utils.PasswordFileFlag,
					utils.LightKDFFlag,
				},
				ArgsUsage: "<keyFile>",
				Description: `
    geth account import <keyfile>

Imports an unencrypted private key from <keyfile> and creates a new account.
Prints the address.

The keyfile is assumed to contain an unencrypted private key in hexadecimal format.

The account is saved in encrypted format, you are prompted for a password.

You must remember this password to unlock your account in the future.

For non-interactive use the password can be specified with the -password flag:

    geth account import [options] <keyfile>

Note:
As you can directly copy your encrypted accounts to another ethereum instance,
this import mechanism is not needed when you transfer an account between
nodes.
`,
				// geth account import <keyfile>

				// 暗号化されていない秘密鍵を<keyfile>からインポートし、新しいアカウントを作成します。
				// アドレスを出力します。
				// キーファイルには、16進形式の暗号化されていない秘密鍵が含まれていると見なされます。
				// アカウントは暗号化された形式で保存され、パスワードの入力を求められます。
				// 今後アカウントのロックを解除するには、このパスワードを覚えておく必要があります。
				// 非対話型の使用の場合、パスワードは-passwordフラグで指定できます。
				//     geth account import [options] <keyfile>
				// ノート：
				// 暗号化されたアカウントを別のイーサリアムインスタンスに直接コピーできるため、ノード間でアカウントを転送する場合、このインポートメカニズムは必要ありません。
			},
		},
	}
)

func accountList(ctx *cli.Context) error {
	stack, _ := makeConfigNode(ctx)
	var index int
	for _, wallet := range stack.AccountManager().Wallets() {
		for _, account := range wallet.Accounts() {
			fmt.Printf("Account #%d: {%x} %s\n", index, account.Address, &account.URL)
			index++
		}
	}
	return nil
}

// tries unlocking the specified account a few times.
// 指定されたアカウントのロックを数回試みます。
func unlockAccount(ks *keystore.KeyStore, address string, i int, passwords []string) (accounts.Account, string) {
	account, err := utils.MakeAddress(ks, address)
	if err != nil {
		utils.Fatalf("Could not list accounts: %v", err) // アカウントを一覧表示できませんでした：
	}
	for trials := 0; trials < 3; trials++ {
		prompt := fmt.Sprintf("Unlocking account %s | Attempt %d/%d", address, trials+1, 3)
		password := utils.GetPassPhraseWithList(prompt, false, i, passwords)
		err = ks.Unlock(account, password)
		if err == nil {
			log.Info("Unlocked account", "address", account.Address.Hex())
			return account, password
		}
		if err, ok := err.(*keystore.AmbiguousAddrError); ok {
			log.Info("Unlocked account", "address", account.Address.Hex())
			return ambiguousAddrRecovery(ks, err, password), password
		}
		if err != keystore.ErrDecrypt {
			// No need to prompt again if the error is not decryption-related.
			// エラーが復号化に関連していない場合は、再度プロンプトを表示する必要はありません。
			break
		}
	}
	// All trials expended to unlock account, bail out
	// アカウントのロックを解除し、救済するために費やされたすべてのトライアル
	utils.Fatalf("Failed to unlock account %s (%v)", address, err)

	return accounts.Account{}, ""
}

func ambiguousAddrRecovery(ks *keystore.KeyStore, err *keystore.AmbiguousAddrError, auth string) accounts.Account {
	fmt.Printf("Multiple key files exist for address %x:\n", err.Addr) // アドレス用に複数のキーファイルが存在します
	for _, a := range err.Matches {
		fmt.Println("  ", a.URL)
	}
	fmt.Println("Testing your password against all of them...") // それらすべてに対してパスワードをテストしています...
	var match *accounts.Account
	for _, a := range err.Matches {
		if err := ks.Unlock(a, auth); err == nil {
			match = &a
			break
		}
	}
	if match == nil {
		utils.Fatalf("None of the listed files could be unlocked.") // リストされたファイルはどれもロックを解除できませんでした。
	}
	fmt.Printf("Your password unlocked %s\n", match.URL)                                                 // パスワードのロックが解除されました
	fmt.Println("In order to avoid this warning, you need to remove the following duplicate key files:") // この警告を回避するには、次の重複するキーファイルを削除する必要があります。
	for _, a := range err.Matches {
		if a != *match {
			fmt.Println("  ", a.URL)
		}
	}
	return *match
}

// accountCreate creates a new account into the keystore defined by the CLI flags.
// accountCreateは、CLIフラグで定義されたキーストアに新しいアカウントを作成します。
func accountCreate(ctx *cli.Context) error {
	cfg := gethConfig{Node: defaultNodeConfig()}
	// Load config file.
	// 構成ファイルをロードします。
	if file := ctx.GlobalString(configFileFlag.Name); file != "" {
		if err := loadConfig(file, &cfg); err != nil {
			utils.Fatalf("%v", err)
		}
	}
	utils.SetNodeConfig(ctx, &cfg.Node)
	keydir, err := cfg.Node.KeyDirConfig()
	if err != nil {
		utils.Fatalf("Failed to read configuration: %v", err) // 構成の読み取りに失敗しました：
	}
	scryptN := keystore.StandardScryptN
	scryptP := keystore.StandardScryptP
	if cfg.Node.UseLightweightKDF {
		scryptN = keystore.LightScryptN
		scryptP = keystore.LightScryptP
	}
	// 新しいアカウントはパスワードでロックされています。パスワードを教えてください。このパスワードを忘れないでください。
	password := utils.GetPassPhraseWithList("Your new account is locked with a password. Please give a password. Do not forget this password.", true, 0, utils.MakePasswordList(ctx))

	account, err := keystore.StoreKey(keydir, password, scryptN, scryptP)

	if err != nil {
		utils.Fatalf("Failed to create account: %v", err)
	}
	fmt.Printf("\nYour new key was generated\n\n")
	fmt.Printf("Public address of the key:   %s\n", account.Address.Hex())
	fmt.Printf("Path of the secret key file: %s\n\n", account.URL.Path)
	fmt.Printf("- You can share your public address with anyone. Others need it to interact with you.\n")
	fmt.Printf("- You must NEVER share the secret key with anyone! The key controls access to your funds!\n")
	fmt.Printf("- You must BACKUP your key file! Without the key, it's impossible to access account funds!\n")
	fmt.Printf("- You must REMEMBER your password! Without the password, it's impossible to decrypt the key!\n\n")
	// 新しいキーが生成されました
	// キーのパブリックアドレス：
	// 秘密鍵ファイルのパス：
	//  -公開アドレスは誰とでも共有できます。他の人はあなたと対話するためにそれを必要とします。
	//  -秘密鍵を他人と共有してはいけません！キーはあなたの資金へのアクセスを制御します！
	//  -キーファイルをバックアップする必要があります！キーがないと、口座の資金にアクセスすることは不可能です！
	//  -パスワードを覚えておく必要があります！パスワードがないと、キーを復号化することはできません。
	return nil
}

// accountUpdate transitions an account from a previous format to the current
// one, also providing the possibility to change the pass-phrase.
// accountUpdateは、アカウントを以前の形式から現在の形式に移行し、パスフレーズを変更する可能性も提供します。
func accountUpdate(ctx *cli.Context) error {
	if len(ctx.Args()) == 0 {
		utils.Fatalf("No accounts specified to update") // 更新するアカウントが指定されていません
	}
	stack, _ := makeConfigNode(ctx)
	ks := stack.AccountManager().Backends(keystore.KeyStoreType)[0].(*keystore.KeyStore)

	for _, addr := range ctx.Args() {
		account, oldPassword := unlockAccount(ks, addr, 0, nil)
		newPassword := utils.GetPassPhraseWithList("Please give a new password. Do not forget this password.", true, 0, nil) //	新しいパスワードを入力してください。このパスワードを忘れないでください。
		if err := ks.Update(account, oldPassword, newPassword); err != nil {
			utils.Fatalf("Could not update the account: %v", err) // アカウントを更新できませんでした
		}
	}
	return nil
}

func importWallet(ctx *cli.Context) error {
	keyfile := ctx.Args().First()
	if len(keyfile) == 0 {
		utils.Fatalf("keyfile must be given as argument") // キーファイルは引数として指定する必要があります
	}
	keyJSON, err := ioutil.ReadFile(keyfile)
	if err != nil {
		utils.Fatalf("Could not read wallet file: %v", err) // ウォレットファイルを読み取れませんでした：
	}

	stack, _ := makeConfigNode(ctx)
	passphrase := utils.GetPassPhraseWithList("", false, 0, utils.MakePasswordList(ctx))

	ks := stack.AccountManager().Backends(keystore.KeyStoreType)[0].(*keystore.KeyStore)
	acct, err := ks.ImportPreSaleKey(keyJSON, passphrase)
	if err != nil {
		utils.Fatalf("%v", err)
	}
	fmt.Printf("Address: {%x}\n", acct.Address)
	return nil
}

func accountImport(ctx *cli.Context) error {
	keyfile := ctx.Args().First()
	if len(keyfile) == 0 {
		utils.Fatalf("keyfile must be given as argument")
	}
	key, err := crypto.LoadECDSA(keyfile)
	if err != nil {
		utils.Fatalf("Failed to load the private key: %v", err)
	}
	stack, _ := makeConfigNode(ctx)
	passphrase := utils.GetPassPhraseWithList("Your new account is locked with a password. Please give a password. Do not forget this password.", true, 0, utils.MakePasswordList(ctx))

	ks := stack.AccountManager().Backends(keystore.KeyStoreType)[0].(*keystore.KeyStore)
	acct, err := ks.ImportECDSA(key, passphrase)
	if err != nil {
		utils.Fatalf("Could not create the account: %v", err)
	}
	fmt.Printf("Address: {%x}\n", acct.Address)
	return nil
}
