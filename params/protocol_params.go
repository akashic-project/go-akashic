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

package params

import "math/big"

const (
	GasLimitBoundDivisor uint64 = 1024               // 更新計算で使用される、ガス制限の限界除数。       // The bound divisor of the gas limit, used in update calculations.
	MinGasLimit          uint64 = 5000               // ガス制限の最小値はこれまでにある可能性があります。// Minimum the gas limit may ever be.
	MaxGasLimit          uint64 = 0x7fffffffffffffff // ガス制限の最大値（2 ^ 63-1）。// Maximum the gas limit (2^63-1).
	GenesisGasLimit      uint64 = 4712388            // Genesisブロックのガス制限。   // Gas limit of the Genesis block.

	MaximumExtraDataSize  uint64 = 32    // 最大サイズの追加データはGenesisの後にある可能性があります。// Maximum size extra data may be after Genesis.
	ExpByteGas            uint64 = 10    // EXP命令の時間ceil（log256（exponent））。               // Times ceil(log256(exponent)) for the EXP instruction.
	SloadGas              uint64 = 50    // * COPY操作でコピー（切り上げ）されて追加される32バイトワードの数を掛けます。// Multiplied by the number of 32-byte words that are copied (round up) for any *COPY operation and added.
	CallValueTransferGas  uint64 = 9000  // 値の転送がゼロ以外の場合、CALLに対して支払われます。      // Paid for CALL when the value transfer is non-zero.
	CallNewAccountGas     uint64 = 25000 // 宛先アドレスが以前に存在しなかった場合にCALLに対して支払われます。// Paid for CALL when the destination address didn't exist prior.
	TxGas                 uint64 = 21000 // トランザクションごとにコントラクトを作成しません。注：トランザクション間の通話のデータについては支払われません。 // Per transaction not creating a contract. NOTE: Not payable on data of calls between transactions.
	TxGasContractCreation uint64 = 53000 // コントラクトを作成するトランザクションごと。注：トランザクション間の通話のデータについては支払われません。      // Per transaction that creates a contract. NOTE: Not payable on data of calls between transactions.
	TxDataZeroGas         uint64 = 4     // ゼロに等しいトランザクションに添付されたデータのバイトごと。注：トランザクション間の通話のデータについては支払われません。 // Per byte of data attached to a transaction that equals zero. NOTE: Not payable on data of calls between transactions.
	QuadCoeffDiv          uint64 = 512   // メモリコスト方程式の2次粒子の除数。 // Divisor for the quadratic particle of the memory cost equation.
	LogDataGas            uint64 = 8     // LOG *操作のデータのバイトごと。    // Per byte in a LOG* operation's data.
	CallStipend           uint64 = 2300  // 通話開始時にガスを放出します。     // Free gas given at beginning of call.

	Keccak256Gas     uint64 = 30 // KECCAK256操作ごとに1回。// Once per KECCAK256 operation.
	Keccak256WordGas uint64 = 6  // KECCAK256操作のデータのワードごとに1回。 // Once per word of the KECCAK256 operation's data.

	SstoreSetGas    uint64 = 20000 // SSTORE操作ごとに1回。// Once per SSTORE operation.
	SstoreResetGas  uint64 = 5000  // ゼロネスがゼロから変化した場合、SSTORE操作ごとに1回。// Once per SSTORE operation if the zeroness changes from zero.
	SstoreClearGas  uint64 = 5000  // ゼロ度が変わらない場合は、SSTORE操作ごとに1回。      // Once per SSTORE operation if the zeroness doesn't change.
	SstoreRefundGas uint64 = 15000 // ゼロネスがゼロに変更された場合、SSTORE操作ごとに1回。// Once per SSTORE operation if the zeroness changes to zero.

	NetSstoreNoopGas  uint64 = 200   // 値が変更されない場合は、SSTORE操作ごとに1回。    // Once per SSTORE operation if the value doesn't change.
	NetSstoreInitGas  uint64 = 20000 // クリーンゼロからのSSTORE操作ごとに1回。    // Once per SSTORE operation from clean zero.
	NetSstoreCleanGas uint64 = 5000  // ゼロ以外のクリーンからSSTORE操作ごとに1回。// Once per SSTORE operation from clean non-zero.
	NetSstoreDirtyGas uint64 = 200   // ダーティからのSSTORE操作ごとに1回。       // Once per SSTORE operation from dirty.

	NetSstoreClearRefund      uint64 = 15000 // 元々存在していたストレージスロットをクリアするためのSSTORE操作ごとに1回 // Once per SSTORE operation for clearing an originally existing storage slot
	NetSstoreResetRefund      uint64 = 4800  // 元のゼロ以外の値にリセットするためのSSTORE操作ごとに1回 // Once per SSTORE operation for resetting to the original non-zero value
	NetSstoreResetClearRefund uint64 = 19800 // 元のゼロ値にリセットするためのSSTORE操作ごとに1回      // Once per SSTORE operation for resetting to the original zero value

	SstoreSentryGasEIP2200            uint64 = 2300  // SSTORE呼び出しに存在する必要がある最小ガスであり、消費されない // Minimum gas required to be present for an SSTORE call, not consumed
	SstoreSetGasEIP2200               uint64 = 20000 // クリーンゼロから非ゼロまでのSSTORE操作ごとに1回              // Once per SSTORE operation from clean zero to non-zero
	SstoreResetGasEIP2200             uint64 = 5000  // ゼロ以外のクリーンから他の何かへのSSTORE操作ごとに1回        // Once per SSTORE operation from clean non-zero to something else
	SstoreClearsScheduleRefundEIP2200 uint64 = 15000 // 元々存在していたストレージスロットをクリアするためのSSTORE操作ごとに1回 // Once per SSTORE operation for clearing an originally existing storage slot

	ColdAccountAccessCostEIP2929 = uint64(2600) // COLD_ACCOUNT_ACCESS_COST
	ColdSloadCostEIP2929         = uint64(2100) // COLD_SLOAD_COST
	WarmStorageReadCostEIP2929   = uint64(100)  // WARM_STORAGE_READ_COST

	// In EIP-2200: SstoreResetGas was 5000.
	// In EIP-2929: SstoreResetGas was changed to '5000 - COLD_SLOAD_COST'.
	// In EIP-3529: SSTORE_CLEARS_SCHEDULE is defined as SSTORE_RESET_GAS + ACCESS_LIST_STORAGE_KEY_COST
	// Which becomes: 5000 - 2100 + 1900 = 4800
	SstoreClearsScheduleRefundEIP3529 uint64 = SstoreResetGasEIP2200 - ColdSloadCostEIP2929 + TxAccessListStorageKeyGas

	JumpdestGas   uint64 = 1     // JUMPDEST操作ごとに1回。            // Once per JUMPDEST operation.
	EpochDuration uint64 = 30000 // プルーフオブワークエポック間の期間。 // Duration between proof-of-work epochs.

	CreateDataGas         uint64 = 200   //
	CallCreateDepth       uint64 = 1024  // 呼び出し/作成スタックの最大深度。 // Maximum depth of call/create stack.
	ExpGas                uint64 = 10    // EXP命令ごとに1回 // Once per EXP instruction
	LogGas                uint64 = 375   // LOG *操作ごと。 // Per LOG* operation.
	CopyGas               uint64 = 3     //
	StackLimit            uint64 = 1024  // 許可されるVMスタックの最大サイズ。// Maximum size of VM stack allowed.
	TierStepGas           uint64 = 0     // 操作ごとに1回、それらを選択します。// Once per operation, for a selection of them.
	LogTopicGas           uint64 = 375   // LOGトランザクションごとに、LOG *の*を掛けます。例えばLOG0では0 * c_txLogTopicGasが発生し、LOG4では4 * c_txLogTopicGasが発生します。// Multiplied by the * of the LOG*, per LOG transaction. e.g. LOG0 incurs 0 * c_txLogTopicGas, LOG4 incurs 4 * c_txLogTopicGas.
	CreateGas             uint64 = 32000 // CREATE操作およびコントラクト作成トランザクションごとに1回。// Once per CREATE operation & contract-creation transaction.
	Create2Gas            uint64 = 32000 // CREATE2操作ごとに1回 // Once per CREATE2 operation
	SelfdestructRefundGas uint64 = 24000 // 自己破壊操作の後に返金されます。// Refunded following a selfdestruct operation.
	MemoryGas             uint64 = 3     //（メモリ内の参照される最上位バイト+1）のアドレスの時間を計測します。注：参照は、読み取り、書き込み、およびRETURNやCALLなどの命令で行われます// Times the address of the (highest referenced byte in memory + 1). NOTE: referencing happens on read, write and in instructions such as RETURN and CALL.

	TxDataNonZeroGasFrontier  uint64 = 68   // ゼロに等しくないトランザクションに添付されたデータのバイトごと。注：トランザクション間の通話のデータについては支払われません。 // Per byte of data attached to a transaction that is not equal to zero. NOTE: Not payable on data of calls between transactions.
	TxDataNonZeroGasEIP2028   uint64 = 16   // EIP 2028以降のトランザクションに添付されたゼロ以外のデータのバイトあたり（イスタンブールの一部） // Per byte of non zero data attached to a transaction after EIP 2028 (part in Istanbul)
	TxAccessListAddressGas    uint64 = 2400 // EIP2930アクセスリストで指定されたアドレスごと // Per address specified in EIP 2930 access list
	TxAccessListStorageKeyGas uint64 = 1900 // EIP2930アクセスリストで指定されたストレージキーごと // Per storage key specified in EIP 2930 access list

	// These have been changed during the course of the chain
	// これらは、チェーンの途中で変更されました
	CallGasFrontier              uint64 = 40  // CALL操作およびメッセージ呼び出しトランザクションごとに1回。   // Once per CALL operation & message call transaction.
	CallGasEIP150                uint64 = 700 // CALLのガスの静的部分-EIP 150（タンジェリン）の後に派生します // Static portion of gas for CALL-derivates after EIP 150 (Tangerine)
	BalanceGasFrontier           uint64 = 20  // BALANCE操作のコスト // The cost of a BALANCE operation
	BalanceGasEIP150             uint64 = 400 // タンジェリン後のBALANCE操作のコスト // The cost of a BALANCE operation after Tangerine
	BalanceGasEIP1884            uint64 = 700 // EIP 1884（イスタンブールの一部）後のBALANCE操作のコスト // The cost of a BALANCE operation after EIP 1884 (part of Istanbul)
	ExtcodeSizeGasFrontier       uint64 = 20  // EIP 150（タンジェリン）前のEXTCODESIZEのコスト // Cost of EXTCODESIZE before EIP 150 (Tangerine)
	ExtcodeSizeGasEIP150         uint64 = 700 // EIP 150後のEXTCODESIZEのコスト（タンジェリン） // Cost of EXTCODESIZE after EIP 150 (Tangerine)
	SloadGasFrontier             uint64 = 50
	SloadGasEIP150               uint64 = 200
	SloadGasEIP1884              uint64 = 800  // EIP 1884後のSLOADのコスト（イスタンブールの一部         // Cost of SLOAD after EIP 1884 (part of Istanbul)
	SloadGasEIP2200              uint64 = 800  // EIP 2200後のSLOADのコスト（イスタンブールの一部）       // Cost of SLOAD after EIP 2200 (part of Istanbul)
	ExtcodeHashGasConstantinople uint64 = 400  // EXTCODEHASHのコスト（コンスタンティノープルで導入）     // Cost of EXTCODEHASH (introduced in Constantinople)
	ExtcodeHashGasEIP1884        uint64 = 700  // EIP 1884後のEXTCODEHASHのコスト（イスタンブールの一部） // Cost of EXTCODEHASH after EIP 1884 (part in Istanbul)
	SelfdestructGasEIP150        uint64 = 5000 // EIP 150後のSELFDESTRUCTのコスト（タンジェリン）        // Cost of SELFDESTRUCT post EIP 150 (Tangerine)

	// EXP has a dynamic portion depending on the size of the exponent
	// EXPには、指数のサイズに応じて動的な部分があります
	ExpByteFrontier uint64 = 10 // フロンティアで10に設定されました                      // was set to 10 in Frontier
	ExpByteEIP158   uint64 = 50 // Eip158（スプリアスドラゴン）中に50に引き上げられました // was raised to 50 during Eip158 (Spurious Dragon)

	// Extcodecopy has a dynamic AND a static cost. This represents only the
	// static portion of the gas. It was changed during EIP 150 (Tangerine)
	// Extcodecopyには、動的および静的コストがあります。これは、ガスの静的部分のみを表します。
	// EIP 150（タンジェリン）中に変更されました
	ExtcodeCopyBaseFrontier uint64 = 20
	ExtcodeCopyBaseEIP150   uint64 = 700

	// CreateBySelfdestructGas is used when the refunded account is one that does
	// not exist. This logic is similar to call.
	// Introduced in Tangerine Whistle (Eip 150)
	// CreateBySelfdestructGasは、返金されたアカウントが存在しないアカウントである場合に使用されます。
	// このロジックは呼び出しに似ています。
	// タンジェリンホイッスルで紹介（Eip 150）
	CreateBySelfdestructGas uint64 = 25000

	BaseFeeChangeDenominator = 8          // 基本料金がブロック間で変更できる金額を制限します。           // Bounds the amount the base fee can change between blocks.
	ElasticityMultiplier     = 2          // EIP-1559ブロックが持つ可能性のある最大ガス制限を制限します。 // Bounds the maximum gas limit an EIP-1559 block may have.
	InitialBaseFee           = 1000000000 // EIP-1559ブロックの初期基本料金。                          // Initial base fee for EIP-1559 blocks.

	MaxCodeSize = 24576 // 契約に許可する最大バイトコード // Maximum bytecode to permit for a contract

	// Precompiled contract gas prices
	// コンパイル済みの契約ガス価格

	EcrecoverGas        uint64 = 3000 // 楕円曲線センダー回収ガス価格 // Elliptic curve sender recovery gas price
	Sha256BaseGas       uint64 = 60   // SHA256操作の基本価格        // Base price for a SHA256 operation
	Sha256PerWordGas    uint64 = 12   // SHA256操作のワードあたりの価格 // Per-word price for a SHA256 operation
	Ripemd160BaseGas    uint64 = 600  // RIPEMD160操作の基本価格       // Base price for a RIPEMD160 operation
	Ripemd160PerWordGas uint64 = 120  // RIPEMD160操作のワードあたりの価格 // Per-word price for a RIPEMD160 operation
	IdentityBaseGas     uint64 = 15   // データコピー操作の基本価格 // Base price for a data copy operation
	IdentityPerWordGas  uint64 = 3    // データコピー操作の作業あたりの価格 // Per-work price for a data copy operation

	Bn256AddGasByzantium             uint64 = 500    // 楕円曲線の追加に必要なビザンチウムガス         // Byzantium gas needed for an elliptic curve addition
	Bn256AddGasIstanbul              uint64 = 150    // 楕円曲線の追加に必要なガス                    // Gas needed for an elliptic curve addition
	Bn256ScalarMulGasByzantium       uint64 = 40000  // 楕円曲線のスカラー乗法に必要なビザンチウムガス　// Byzantium gas needed for an elliptic curve scalar multiplication
	Bn256ScalarMulGasIstanbul        uint64 = 6000   // 楕円曲線のスカラー乗法に必要なガス             // Gas needed for an elliptic curve scalar multiplication
	Bn256PairingBaseGasByzantium     uint64 = 100000 // 楕円曲線ペアリングチェックのビザンチウム基本価格 // Byzantium base price for an elliptic curve pairing check
	Bn256PairingBaseGasIstanbul      uint64 = 45000  // 楕円曲線ペアリングチェックの基本価格            // Base price for an elliptic curve pairing check
	Bn256PairingPerPointGasByzantium uint64 = 80000  // 楕円曲線ペアリングチェックのビザンチウムポイントあたりの価格 // Byzantium per-point price for an elliptic curve pairing check
	Bn256PairingPerPointGasIstanbul  uint64 = 34000  //楕円曲線ペアリングチェックのポイントあたりの価格             // Per-point price for an elliptic curve pairing check

	Bls12381G1AddGas          uint64 = 600    // BLS12-381楕円曲線G1ポイント追加の価格        // Price for BLS12-381 elliptic curve G1 point addition
	Bls12381G1MulGas          uint64 = 12000  // BLS12-381楕円曲線G1ポイントスカラー乗法の価格 // Price for BLS12-381 elliptic curve G1 point scalar multiplication
	Bls12381G2AddGas          uint64 = 4500   // BLS12-381楕円曲線G2ポイント追加の価格        // Price for BLS12-381 elliptic curve G2 point addition
	Bls12381G2MulGas          uint64 = 55000  // BLS12-381楕円曲線G2ポイントスカラー乗法の価格 // Price for BLS12-381 elliptic curve G2 point scalar multiplication
	Bls12381PairingBaseGas    uint64 = 115000 // BLS12-381楕円曲線ペアリングチェックの基本ガス価格 // Base gas price for BLS12-381 elliptic curve pairing check
	Bls12381PairingPerPairGas uint64 = 23000  // BLS12-381楕円曲線ペアリングチェックのポイントごとのペアガス価格 // Per-point pair gas price for BLS12-381 elliptic curve pairing check
	Bls12381MapG1Gas          uint64 = 5500   // フィールド要素をG1操作にマッピングするBLS12-381のガス価格      // Gas price for BLS12-381 mapping field element to G1 operation
	Bls12381MapG2Gas          uint64 = 110000 // フィールド要素をG2操作にマッピングするBLS12-381のガス価格      // Gas price for BLS12-381 mapping field element to G2 operation

	// The Refund Quotient is the cap on how much of the used gas can be refunded. Before EIP-3529,
	// up to half the consumed gas could be refunded. Redefined as 1/5th in EIP-3529
	// 払い戻し商は、使用済みガスの払い戻しが可能な金額の上限です。 EIP-3529以前は、消費ガスの最大半分が払い戻される可能性がありました。
	//  EIP-3529で1/5として再定義
	RefundQuotient        uint64 = 2
	RefundQuotientEIP3529 uint64 = 5
)

// Gas discount table for BLS12-381 G1 and G2 multi exponentiation operations
// BLS12-381G1およびG2の複数のべき乗演算のガス割引表
var Bls12381MultiExpDiscountTable = [128]uint64{1200, 888, 764, 641, 594, 547, 500, 453, 438, 423, 408, 394, 379, 364, 349, 334, 330, 326, 322, 318, 314, 310, 306, 302, 298, 294, 289, 285, 281, 277, 273, 269, 268, 266, 265, 263, 262, 260, 259, 257, 256, 254, 253, 251, 250, 248, 247, 245, 244, 242, 241, 239, 238, 236, 235, 233, 232, 231, 229, 228, 226, 225, 223, 222, 221, 220, 219, 219, 218, 217, 216, 216, 215, 214, 213, 213, 212, 211, 211, 210, 209, 208, 208, 207, 206, 205, 205, 204, 203, 202, 202, 201, 200, 199, 199, 198, 197, 196, 196, 195, 194, 193, 193, 192, 191, 191, 190, 189, 188, 188, 187, 186, 185, 185, 184, 183, 182, 182, 181, 180, 179, 179, 178, 177, 176, 176, 175, 174}

var (
	DifficultyBoundDivisor = big.NewInt(2048)   // 更新計算で使用される、難易度の限界除数。   // The bound divisor of the difficulty, used in the update calculations.
	GenesisDifficulty      = big.NewInt(131072) // Genesisブロックの難易度。                // Difficulty of the Genesis block.
	MinimumDifficulty      = big.NewInt(131072) // 難易度がこれまでにある可能性のある最小値。 // The minimum that the difficulty may ever be.
	DurationLimit          = big.NewInt(13)     // 難易度を上げるかどうかを決定するために使用されるブロック時間の決定境界。 // The decision boundary on the blocktime duration used to determine whether difficulty should go up or not.
)
