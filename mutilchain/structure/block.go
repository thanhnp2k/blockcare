package structure

type MsgBlock struct {
	Hash         string
	Height       int64
	Time         int64
	Nonce        uint64
	Size         float64
	Mtx          TxInfo
	Difficulty   int64
	Transactions []TxInfo
	PrevHash     string
	Tx_Count     int
}

type TxOut struct {
	Value    int64
	Address  string
	PkScript []byte
}

type OutPoint struct {
	Hash  string
	Index uint32
}

type TxIn struct {
	Value            int64
	Time             int64
	BlockHeight      int64
	PreviousOutPoint OutPoint
	SignatureScript  []byte
	Sequence         uint32
}

type TxInfo struct {
	Hex          string // raw tx
	Height       int64
	Time         int64   // timestamp
	Hash         string  // hash for hash
	Txid         string  // prefix hash
	Version      int     // version of tx
	Size         float64 // size of tx in KB
	Fee          string  // fee in TX
	Feeuint64    uint64  // fee in atomic units
	TXpublickey  string
	ValidBlock   string   // the tx is valid in which block
	InvalidBlock []string // the tx is invalid in which block
	In           int      // inputs counts
	Out          int      // outputs counts
	Amount       string
	CoinBase     bool // is tx coin base
	LockTime     int64
	TxIn         []TxIn
	TxOut        []TxOut
	Skipped      bool
}
