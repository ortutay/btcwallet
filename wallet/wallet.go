/*
 * Copyright (c) 2013, 2014 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package wallet

import (
	"bytes"
	"code.google.com/p/go.crypto/ripemd160"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/conformal/btcec"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
	"github.com/davecgh/go-spew/spew"
	"io"
	"math/big"
	"sync"
	"time"
)

const (
	// Length in bytes of KDF output.
	kdfOutputBytes = 32

	// Maximum length in bytes of a comment that can have a size represented
	// as a uint16.
	maxCommentLen = (1 << 16) - 1
)

const (
	defaultKdfComputeTime = 0.25
	defaultKdfMaxMem      = 32 * 1024 * 1024
)

// Possible errors when dealing with wallets.
var (
	ErrAddressNotFound    = errors.New("address not found")
	ErrChecksumMismatch   = errors.New("checksum mismatch")
	ErrDuplicate          = errors.New("duplicate key or address")
	ErrMalformedEntry     = errors.New("malformed entry")
	ErrNetworkMismatch    = errors.New("network mismatch")
	ErrWalletDoesNotExist = errors.New("non-existant wallet")
	ErrWalletLocked       = errors.New("wallet is locked")
)

var (
	// '\xbaWALLET\x00'
	fileID = [8]byte{0xba, 0x57, 0x41, 0x4c, 0x4c, 0x45, 0x54, 0x00}

	mainnetMagicBytes = [4]byte{0xf9, 0xbe, 0xb4, 0xd9}
	testnetMagicBytes = [4]byte{0x0b, 0x11, 0x09, 0x07}
)

type entryHeader byte

const (
	addrCommentHeader entryHeader = 1 << iota
	txCommentHeader
	deletedHeader
	addrHeader entryHeader = 0
)

// We want to use binaryRead and binaryWrite instead of binary.Read
// and binary.Write because those from the binary package do not return
// the number of bytes actually written or read.  We need to return
// this value to correctly support the io.ReaderFrom and io.WriterTo
// interfaces.
func binaryRead(r io.Reader, order binary.ByteOrder, data interface{}) (n int64, err error) {
	var read int
	buf := make([]byte, binary.Size(data))
	if read, err = r.Read(buf); err != nil {
		return int64(read), err
	}
	if read < binary.Size(data) {
		return int64(read), io.EOF
	}
	return int64(read), binary.Read(bytes.NewBuffer(buf), order, data)
}

// See comment for binaryRead().
func binaryWrite(w io.Writer, order binary.ByteOrder, data interface{}) (n int64, err error) {
	var buf bytes.Buffer
	if err = binary.Write(&buf, order, data); err != nil {
		return 0, err
	}

	written, err := w.Write(buf.Bytes())
	return int64(written), err
}

// pubkeyFromPrivkey creates an encoded pubkey based on a
// 32-byte privkey.  The returned pubkey is 33 bytes if compressed,
// or 65 bytes if uncompressed.
func pubkeyFromPrivkey(privkey []byte, compress bool) (pubkey []byte) {
	x, y := btcec.S256().ScalarBaseMult(privkey)
	pub := (*btcec.PublicKey)(&ecdsa.PublicKey{
		Curve: btcec.S256(),
		X:     x,
		Y:     y,
	})

	if compress {
		return pub.SerializeCompressed()
	}
	return pub.SerializeUncompressed()
}

func keyOneIter(passphrase, salt []byte, memReqts uint64) []byte {
	saltedpass := append(passphrase, salt...)
	lutbl := make([]byte, memReqts)

	// Seed for lookup table
	seed := sha512.Sum512(saltedpass)
	copy(lutbl[:sha512.Size], seed[:])

	for nByte := 0; nByte < (int(memReqts) - sha512.Size); nByte += sha512.Size {
		hash := sha512.Sum512(lutbl[nByte : nByte+sha512.Size])
		copy(lutbl[nByte+sha512.Size:nByte+2*sha512.Size], hash[:])
	}

	x := lutbl[cap(lutbl)-sha512.Size:]

	seqCt := uint32(memReqts / sha512.Size)
	nLookups := seqCt / 2
	for i := uint32(0); i < nLookups; i++ {
		// Armory ignores endianness here.  We assume LE.
		newIdx := binary.LittleEndian.Uint32(x[cap(x)-4:]) % seqCt

		// Index of hash result at newIdx
		vIdx := newIdx * sha512.Size
		v := lutbl[vIdx : vIdx+sha512.Size]

		// XOR hash x with hash v
		for j := 0; j < sha512.Size; j++ {
			x[j] ^= v[j]
		}

		// Save new hash to x
		hash := sha512.Sum512(x)
		copy(x, hash[:])
	}

	return x[:kdfOutputBytes]
}

// Key implements the key derivation function used by Armory
// based on the ROMix algorithm described in Colin Percival's paper
// "Stronger Key Derivation via Sequential Memory-Hard Functions"
// (http://www.tarsnap.com/scrypt/scrypt.pdf).
func Key(passphrase []byte, params *kdfParameters) []byte {
	masterKey := passphrase
	for i := uint32(0); i < params.nIter; i++ {
		masterKey = keyOneIter(masterKey, params.salt[:], params.mem)
	}
	return masterKey
}

func pad(size int, b []byte) []byte {
	// Prevent a possible panic if the input exceeds the expected size.
	if len(b) > size {
		size = len(b)
	}

	p := make([]byte, size)
	copy(p[size-len(b):], b)
	return p
}

// ChainedPrivKey deterministically generates a new private key using a
// previous address and chaincode.  privkey and chaincode must be 32
// bytes long, and pubkey may either be 33 bytes, 65 bytes or nil (in
// which case it is generated by the privkey).
func ChainedPrivKey(privkey, pubkey, chaincode []byte) ([]byte, error) {
	if len(privkey) != 32 {
		return nil, fmt.Errorf("invalid privkey length %d (must be 32)",
			len(privkey))
	}
	if len(chaincode) != 32 {
		return nil, fmt.Errorf("invalid chaincode length %d (must be 32)",
			len(chaincode))
	}
	if pubkey == nil {
		pubkey = pubkeyFromPrivkey(privkey, true)
	} else if !(len(pubkey) == 65 || len(pubkey) == 33) {
		return nil, fmt.Errorf("invalid pubkey length %d", len(pubkey))
	}

	xorbytes := make([]byte, 32)
	chainMod := sha256.Sum256(pubkey)
	for i := range xorbytes {
		xorbytes[i] = chainMod[i] ^ chaincode[i]
	}
	chainXor := new(big.Int).SetBytes(xorbytes)
	privint := new(big.Int).SetBytes(privkey)

	t := new(big.Int).Mul(chainXor, privint)
	b := t.Mod(t, btcec.S256().N).Bytes()
	return pad(32, b), nil
}

type version struct {
	major         byte
	minor         byte
	bugfix        byte
	autoincrement byte
}

// Enforce that version satisifies the io.ReaderFrom and
// io.WriterTo interfaces.
var _ io.ReaderFrom = &version{}
var _ io.WriterTo = &version{}

// ReaderFromVersion is an io.ReaderFrom and io.WriterTo that
// can specify any particular wallet file format for reading
// depending on the wallet file version.
type ReaderFromVersion interface {
	ReadFromVersion(version, io.Reader) (int64, error)
	io.WriterTo
}

func (v version) String() string {
	str := fmt.Sprintf("%d.%d", v.major, v.minor)
	if v.bugfix != 0x00 || v.autoincrement != 0x00 {
		str += fmt.Sprintf(".%d", v.bugfix)
	}
	if v.autoincrement != 0x00 {
		str += fmt.Sprintf(".%d", v.autoincrement)
	}
	return str
}

func (v version) Uint32() uint32 {
	return uint32(v.major)<<6 | uint32(v.minor)<<4 | uint32(v.bugfix)<<2 | uint32(v.autoincrement)
}

func (v *version) ReadFrom(r io.Reader) (int64, error) {
	// Read 4 bytes for the version.
	versBytes := make([]byte, 4)
	n, err := r.Read(versBytes)
	if err != nil {
		return int64(n), err
	}
	v.major = versBytes[0]
	v.minor = versBytes[1]
	v.bugfix = versBytes[2]
	v.autoincrement = versBytes[3]
	return int64(n), nil
}

func (v *version) WriteTo(w io.Writer) (int64, error) {
	// Write 4 bytes for the version.
	versBytes := []byte{
		v.major,
		v.minor,
		v.bugfix,
		v.autoincrement,
	}
	n, err := w.Write(versBytes)
	return int64(n), err
}

// LT returns whether v is an earlier version than v2.
func (v version) LT(v2 version) bool {
	switch {
	case v.major < v2.major:
		return true

	case v.minor < v2.minor:
		return true

	case v.bugfix < v2.bugfix:
		return true

	case v.autoincrement < v2.autoincrement:
		return true

	default:
		return false
	}
}

// EQ returns whether v2 is an equal version to v.
func (v version) EQ(v2 version) bool {
	switch {
	case v.major != v2.major:
		return false

	case v.minor != v2.minor:
		return false

	case v.bugfix != v2.bugfix:
		return false

	case v.autoincrement != v2.autoincrement:
		return false

	default:
		return true
	}
}

// GT returns whether v is a later version than v2.
func (v version) GT(v2 version) bool {
	switch {
	case v.major > v2.major:
		return true

	case v.minor > v2.minor:
		return true

	case v.bugfix > v2.bugfix:
		return true

	case v.autoincrement > v2.autoincrement:
		return true

	default:
		return false
	}
}

// Various versions.
var (
	// VersArmory is the latest version used by Armory.
	VersArmory = version{1, 35, 0, 0}

	// Vers20LastBlocks is the version where wallet files now hold
	// the 20 most recently seen block hashes.
	Vers20LastBlocks = version{1, 36, 0, 0}

	// VersCurrent is the current wallet file version.
	VersCurrent = Vers20LastBlocks
)

type varEntries []io.WriterTo

func (v *varEntries) WriteTo(w io.Writer) (n int64, err error) {
	ss := []io.WriterTo(*v)

	var written int64
	for _, s := range ss {
		var err error
		if written, err = s.WriteTo(w); err != nil {
			return n + written, err
		}
		n += written
	}
	return n, nil
}

func (v *varEntries) ReadFrom(r io.Reader) (n int64, err error) {
	var read int64

	// Remove any previous entries.
	*v = nil
	wts := []io.WriterTo(*v)

	// Keep reading entries until an EOF is reached.
	for {
		var header entryHeader
		if read, err = binaryRead(r, binary.LittleEndian, &header); err != nil {
			// EOF here is not an error.
			if err == io.EOF {
				return n + read, nil
			}
			return n + read, err
		}
		n += read

		var wt io.WriterTo
		switch header {
		case addrHeader:
			var entry addrEntry
			if read, err = entry.ReadFrom(r); err != nil {
				return n + read, err
			}
			n += read
			wt = &entry
		case addrCommentHeader:
			var entry addrCommentEntry
			if read, err = entry.ReadFrom(r); err != nil {
				return n + read, err
			}
			n += read
			wt = &entry
		case txCommentHeader:
			var entry txCommentEntry
			if read, err = entry.ReadFrom(r); err != nil {
				return n + read, err
			}
			n += read
			wt = &entry
		case deletedHeader:
			var entry deletedEntry
			if read, err = entry.ReadFrom(r); err != nil {
				return n + read, err
			}
			n += read
		default:
			return n, fmt.Errorf("unknown entry header: %d", uint8(header))
		}
		if wt != nil {
			wts = append(wts, wt)
			*v = wts
		}
	}
}

type transactionHashKey string
type comment []byte

// Wallet represents an btcwallet wallet in memory.  It implements
// the io.ReaderFrom and io.WriterTo interfaces to read from and
// write to any type of byte streams, including files.
type Wallet struct {
	net          btcwire.BitcoinNet
	flags        walletFlags
	uniqID       [6]byte
	createDate   int64
	name         [32]byte
	desc         [256]byte
	highestUsed  int64
	kdfParams    kdfParameters
	keyGenerator btcAddress

	// These are non-standard and fit in the extra 1024 bytes between the
	// root address and the appended entries.
	recent recentBlocks

	addrMap        map[btcutil.AddressPubKeyHash]*btcAddress
	addrCommentMap map[btcutil.AddressPubKeyHash]comment
	txCommentMap   map[transactionHashKey]comment

	// The rest of the fields in this struct are not serialized.
	secret struct {
		sync.Mutex
		key []byte
	}
	chainIdxMap   map[int64]*btcutil.AddressPubKeyHash
	importedAddrs []*btcAddress
	lastChainIdx  int64
}

// NewWallet creates and initializes a new Wallet.  name's and
// desc's binary representation must not exceed 32 and 256 bytes,
// respectively.  All address private keys are encrypted with passphrase.
// The wallet is returned unlocked.
func NewWallet(name, desc string, passphrase []byte, net btcwire.BitcoinNet,
	createdAt *BlockStamp, keypoolSize uint) (*Wallet, error) {

	// Check sizes of inputs.
	if len([]byte(name)) > 32 {
		return nil, errors.New("name exceeds 32 byte maximum size")
	}
	if len([]byte(desc)) > 256 {
		return nil, errors.New("desc exceeds 256 byte maximum size")
	}

	// Check for a valid network.
	if !(net == btcwire.MainNet || net == btcwire.TestNet3) {
		return nil, errors.New("wallets must use mainnet or testnet3")
	}

	// Randomly-generate rootkey and chaincode.
	rootkey, chaincode := make([]byte, 32), make([]byte, 32)
	if _, err := rand.Read(rootkey); err != nil {
		return nil, err
	}
	if _, err := rand.Read(chaincode); err != nil {
		return nil, err
	}

	// Create new root address from key and chaincode.
	root, err := newRootBtcAddress(rootkey, nil, chaincode, createdAt)
	if err != nil {
		return nil, err
	}

	// Verify root address keypairs.
	if err := root.verifyKeypairs(); err != nil {
		return nil, err
	}

	// Compute AES key and encrypt root address.
	kdfp, err := computeKdfParameters(defaultKdfComputeTime, defaultKdfMaxMem)
	if err != nil {
		return nil, err
	}
	aeskey := Key([]byte(passphrase), kdfp)
	if err := root.encrypt(aeskey); err != nil {
		return nil, err
	}

	// Create and fill wallet.
	w := &Wallet{
		// TODO(jrick): not sure we will need uniqID, but would be good for
		// compat with armory.
		net: net,
		flags: walletFlags{
			useEncryption: true,
			watchingOnly:  false,
		},
		createDate:   time.Now().Unix(),
		highestUsed:  rootKeyChainIdx,
		kdfParams:    *kdfp,
		keyGenerator: *root,
		recent: recentBlocks{
			lastHeight: createdAt.Height,
			hashes: []*btcwire.ShaHash{
				&createdAt.Hash,
			},
		},
		addrMap:        make(map[btcutil.AddressPubKeyHash]*btcAddress),
		addrCommentMap: make(map[btcutil.AddressPubKeyHash]comment),
		txCommentMap:   make(map[transactionHashKey]comment),
		chainIdxMap:    make(map[int64]*btcutil.AddressPubKeyHash),
		lastChainIdx:   rootKeyChainIdx,
	}
	copy(w.name[:], []byte(name))
	copy(w.desc[:], []byte(desc))

	// Add root address to maps.
	w.addrMap[*w.keyGenerator.address(net)] = &w.keyGenerator
	w.chainIdxMap[rootKeyChainIdx] = w.keyGenerator.address(net)

	// Fill keypool.
	if err := w.extendKeypool(keypoolSize, aeskey, createdAt); err != nil {
		return nil, err
	}

	return w, nil
}

// Name returns the name of a wallet.  This name is used as the
// account name for btcwallet JSON methods.
func (w *Wallet) Name() string {
	last := len(w.name[:])
	for i, b := range w.name[:] {
		if b == 0x00 {
			last = i
			break
		}
	}
	return string(w.name[:last])
}

// ReadFrom reads data from a io.Reader and saves it to a Wallet,
// returning the number of bytes read and any errors encountered.
func (w *Wallet) ReadFrom(r io.Reader) (n int64, err error) {
	var read int64

	w.addrMap = make(map[btcutil.AddressPubKeyHash]*btcAddress)
	w.addrCommentMap = make(map[btcutil.AddressPubKeyHash]comment)
	w.chainIdxMap = make(map[int64]*btcutil.AddressPubKeyHash)
	w.txCommentMap = make(map[transactionHashKey]comment)

	var id [8]byte
	var vers version
	var appendedEntries varEntries

	// Iterate through each entry needing to be read.  If data
	// implements io.ReaderFrom, use its ReadFrom func.  Otherwise,
	// data is a pointer to a fixed sized value.
	datas := []interface{}{
		&id,
		&vers,
		&w.net,
		&w.flags,
		&w.uniqID,
		&w.createDate,
		&w.name,
		&w.desc,
		&w.highestUsed,
		&w.kdfParams,
		make([]byte, 256),
		&w.keyGenerator,
		newUnusedSpace(1024, &w.recent),
		&appendedEntries,
	}
	for _, data := range datas {
		var err error
		switch d := data.(type) {
		case ReaderFromVersion:
			read, err = d.ReadFromVersion(vers, r)

		case io.ReaderFrom:
			read, err = d.ReadFrom(r)

		default:
			read, err = binaryRead(r, binary.LittleEndian, d)
		}
		n += read
		if err != nil {
			return n, err
		}
	}

	if id != fileID {
		return n, errors.New("unknown file ID")
	}

	// Add root address to address map.
	rootAddr := w.keyGenerator.address(w.net)
	w.addrMap[*rootAddr] = &w.keyGenerator
	w.chainIdxMap[rootKeyChainIdx] = rootAddr

	// Fill unserializied fields.
	wts := ([]io.WriterTo)(appendedEntries)
	for _, wt := range wts {
		switch e := wt.(type) {
		case *addrEntry:
			addr := e.addr.address(w.net)
			w.addrMap[*addr] = &e.addr
			if e.addr.chainIndex == importedKeyChainIdx {
				w.importedAddrs = append(w.importedAddrs, &e.addr)
			} else {
				w.chainIdxMap[e.addr.chainIndex] = addr
				if w.lastChainIdx < e.addr.chainIndex {
					w.lastChainIdx = e.addr.chainIndex
				}
			}

		case *addrCommentEntry:
			addr := e.address(w.net)
			w.addrCommentMap[*addr] = comment(e.comment)

		case *txCommentEntry:
			txKey := transactionHashKey(e.txHash[:])
			w.txCommentMap[txKey] = comment(e.comment)

		default:
			return n, errors.New("unknown appended entry")
		}
	}

	return n, nil
}

// WriteTo serializes a Wallet and writes it to a io.Writer,
// returning the number of bytes written and any errors encountered.
func (w *Wallet) WriteTo(wtr io.Writer) (n int64, err error) {
	var wts []io.WriterTo
	var chainedAddrs = make([]io.WriterTo, len(w.chainIdxMap)-1)
	var importedAddrs []io.WriterTo
	for addr, btcAddr := range w.addrMap {
		e := &addrEntry{
			addr: *btcAddr,
		}
		copy(e.pubKeyHash160[:], addr.ScriptAddress())
		if btcAddr.chainIndex >= 0 {
			// Chained addresses are sorted.  This is
			// kind of nice but probably isn't necessary.
			chainedAddrs[btcAddr.chainIndex] = e
		} else if btcAddr.chainIndex == importedKeyChainIdx {
			// No order for imported addresses.
			importedAddrs = append(importedAddrs, e)
		}
	}
	wts = append(chainedAddrs, importedAddrs...)
	for addr, comment := range w.addrCommentMap {
		e := &addrCommentEntry{
			comment: []byte(comment),
		}
		copy(e.pubKeyHash160[:], addr.ScriptAddress())
		wts = append(wts, e)
	}
	for hash, comment := range w.txCommentMap {
		e := &txCommentEntry{
			comment: []byte(comment),
		}
		copy(e.txHash[:], []byte(hash))
		wts = append(wts, e)
	}
	appendedEntries := varEntries(wts)

	// Iterate through each entry needing to be written.  If data
	// implements io.WriterTo, use its WriteTo func.  Otherwise,
	// data is a pointer to a fixed size value.
	datas := []interface{}{
		&fileID,
		&VersCurrent,
		&w.net,
		&w.flags,
		&w.uniqID,
		&w.createDate,
		&w.name,
		&w.desc,
		&w.highestUsed,
		&w.kdfParams,
		make([]byte, 256),
		&w.keyGenerator,
		newUnusedSpace(1024, &w.recent),
		&appendedEntries,
	}
	var written int64
	for _, data := range datas {
		if s, ok := data.(io.WriterTo); ok {
			written, err = s.WriteTo(wtr)
		} else {
			written, err = binaryWrite(wtr, binary.LittleEndian, data)
		}
		n += written
		if err != nil {
			return n, err
		}
	}

	return n, nil
}

// Unlock derives an AES key from passphrase and wallet's KDF
// parameters and unlocks the root key of the wallet.  If
// the unlock was successful, the wallet's secret key is saved,
// allowing the decryption of any encrypted private key.
func (w *Wallet) Unlock(passphrase []byte) error {
	// Derive key from KDF parameters and passphrase.
	key := Key(passphrase, &w.kdfParams)

	// Unlock root address with derived key.
	if _, err := w.keyGenerator.unlock(key); err != nil {
		return err
	}

	// If unlock was successful, save the secret key.
	w.secret.Lock()
	w.secret.key = key
	w.secret.Unlock()
	return nil
}

// Lock performs a best try effort to remove and zero all secret keys
// associated with the wallet.
func (w *Wallet) Lock() (err error) {
	// Remove clear text passphrase from wallet.
	w.secret.Lock()
	if w.secret.key == nil {
		err = ErrWalletLocked
	} else {
		zero(w.secret.key)
		w.secret.key = nil
	}
	w.secret.Unlock()

	// Remove clear text private keys from all address entries.
	for _, addr := range w.addrMap {
		addr.privKeyCT.Lock()
		zero(addr.privKeyCT.key)
		addr.privKeyCT.key = nil
		addr.privKeyCT.Unlock()
	}

	return err
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// IsLocked returns whether a wallet is unlocked (in which case the
// key is saved in memory), or locked.
func (w *Wallet) IsLocked() (locked bool) {
	w.secret.Lock()
	locked = w.secret.key == nil
	w.secret.Unlock()
	return locked
}

// Version returns a wallet's version as a string and int.
func (w *Wallet) Version() (string, int) {
	return "", 0
}

// NextChainedAddress attempts to get the next chained address,
// refilling the keypool if necessary.
func (w *Wallet) NextChainedAddress(bs *BlockStamp,
	keypoolSize uint) (*btcutil.AddressPubKeyHash, error) {

	// Attempt to get address hash of next chained address.
	next160, ok := w.chainIdxMap[w.highestUsed+1]
	if !ok {
		// Extending the keypool requires an unlocked wallet.
		aeskey := make([]byte, 32)
		w.secret.Lock()
		if len(w.secret.key) != 32 {
			w.secret.Unlock()
			return nil, ErrWalletLocked
		}
		copy(aeskey, w.secret.key)
		w.secret.Unlock()

		err := w.extendKeypool(keypoolSize, aeskey, bs)
		if err != nil {
			return nil, err
		}

		next160, ok = w.chainIdxMap[w.highestUsed+1]
		if !ok {
			return nil, errors.New("chain index map inproperly updated")
		}
	}

	// Look up address.
	addr, ok := w.addrMap[*next160]
	if !ok {
		return nil, errors.New("cannot find generated address")
	}

	w.highestUsed++

	// Create and return payment address for address hash.
	return addr.address(w.net), nil
}

// LastChainedAddress returns the most recently requested chained
// address from calling NextChainedAddress, or the root address if
// no chained addresses have been requested.
func (w *Wallet) LastChainedAddress() btcutil.Address {
	return w.chainIdxMap[w.highestUsed]
}

// extendKeypool grows the keypool by n addresses.
func (w *Wallet) extendKeypool(n uint, aeskey []byte, bs *BlockStamp) error {
	// Get last chained address.  New chained addresses will be
	// chained off of this address's chaincode and private key.
	a := w.chainIdxMap[w.lastChainIdx]
	addr, ok := w.addrMap[*a]
	if !ok {
		spew.Dump(a)
		spew.Dump(w.addrMap)
		return errors.New("expected last chained address not found")
	}
	privkey, err := addr.unlock(aeskey)
	if err != nil {
		return err
	}
	cc := addr.chaincode[:]

	// Create n encrypted addresses and add each to the wallet's
	// bookkeeping maps.
	for i := uint(0); i < n; i++ {
		privkey, err = ChainedPrivKey(privkey, addr.pubKey, cc)
		if err != nil {
			return err
		}
		newaddr, err := newBtcAddress(privkey, nil, bs, true)
		if err != nil {
			return err
		}
		if err := newaddr.verifyKeypairs(); err != nil {
			return err
		}
		if err = newaddr.encrypt(aeskey); err != nil {
			return err
		}
		a := newaddr.address(w.net)
		w.addrMap[*a] = newaddr
		newaddr.chainIndex = addr.chainIndex + 1
		w.chainIdxMap[newaddr.chainIndex] = a
		w.lastChainIdx++
		// armory does this.. but all the chaincodes are equal so why
		// not use the root's?
		copy(newaddr.chaincode[:], cc)
		addr = newaddr
	}

	return nil
}

// AddressKey returns the private key for a payment address stored
// in a wallet.  This can fail if the payment address is for a different
// Bitcoin network than what this wallet uses, the address is not
// contained in the wallet, the address does not include a public and
// private key, or if the wallet is locked.
func (w *Wallet) AddressKey(a btcutil.Address) (key *ecdsa.PrivateKey, err error) {
	// Currently, only P2PKH addresses are supported.  This should
	// be extended to a switch-case statement when support for other
	// addresses are added.
	addr, ok := a.(*btcutil.AddressPubKeyHash)
	if !ok {
		return nil, errors.New("unsupported address")
	}

	// Lookup address from map.
	btcaddr, ok := w.addrMap[*addr]
	if !ok {
		return nil, ErrAddressNotFound
	}

	// Both the pubkey and encrypted privkey must be recorded to return
	// the private key.  Error if neither are saved.
	if !btcaddr.flags.hasPubKey {
		return nil, errors.New("no public key for address")
	}
	if !btcaddr.flags.hasPrivKey {
		return nil, errors.New("no private key for address")
	}

	// Parse public key.
	pubkey, err := btcec.ParsePubKey(btcaddr.pubKey, btcec.S256())
	if err != nil {
		return nil, err
	}

	// The wallet's secret will be zeroed on lock, so make a local
	// copy.
	localSecret := make([]byte, 32)
	w.secret.Lock()
	if len(w.secret.key) != 32 {
		w.secret.Unlock()
		return nil, ErrWalletLocked
	}
	copy(localSecret, w.secret.key)
	w.secret.Unlock()

	// Unlock address with wallet secret.  unlock returns a copy of the
	// clear text private key, and may be used safely even during an address
	// lock.
	privKeyCT, err := btcaddr.unlock(localSecret)
	if err != nil {
		return nil, err
	}

	return &ecdsa.PrivateKey{
		PublicKey: *pubkey,
		D:         new(big.Int).SetBytes(privKeyCT),
	}, nil
}

// AddressInfo returns an AddressInfo structure for an address in a wallet.
func (w *Wallet) AddressInfo(a btcutil.Address) (*AddressInfo, error) {
	// Currently, only P2PKH addresses are supported.  This should
	// be extended to a switch-case statement when support for other
	// addresses are added.
	addr, ok := a.(*btcutil.AddressPubKeyHash)
	if !ok {
		return nil, errors.New("unsupported address")
	}

	// Look up address by address hash.
	btcaddr, ok := w.addrMap[*addr]
	if !ok {
		return nil, ErrAddressNotFound
	}

	return btcaddr.info(w.net)
}

// Net returns the bitcoin network identifier for this wallet.
func (w *Wallet) Net() btcwire.BitcoinNet {
	return w.net
}

// SetSyncedWith marks the wallet to be in sync with the block
// described by height and hash.
func (w *Wallet) SetSyncedWith(bs *BlockStamp) {
	// Check if we're trying to rollback the last seen history.
	// If so, and this bs is already saved, remove anything
	// after and return.  Otherwire, remove previous hashes.
	if bs.Height < w.recent.lastHeight {
		maybeIdx := len(w.recent.hashes) - 1 - int(w.recent.lastHeight-bs.Height)
		if maybeIdx >= 0 && maybeIdx < len(w.recent.hashes) &&
			*w.recent.hashes[maybeIdx] == bs.Hash {

			w.recent.lastHeight = bs.Height
			// subslice out the removed hashes.
			w.recent.hashes = w.recent.hashes[:maybeIdx]
			return
		}
		w.recent.hashes = nil
	}

	if bs.Height != w.recent.lastHeight+1 {
		w.recent.hashes = nil
	}

	w.recent.lastHeight = bs.Height
	blockSha := new(btcwire.ShaHash)
	copy(blockSha[:], bs.Hash[:])
	if len(w.recent.hashes) == 20 {
		// Make room for the most recent hash.
		copy(w.recent.hashes, w.recent.hashes[1:])

		// Set new block in the last position.
		w.recent.hashes[19] = blockSha
	} else {
		w.recent.hashes = append(w.recent.hashes, blockSha)
	}
}

// SyncedWith returns the height and hash of the block the wallet is
// currently marked to be in sync with.
func (w *Wallet) SyncedWith() *BlockStamp {
	nHashes := len(w.recent.hashes)
	if nHashes == 0 || w.recent.lastHeight == -1 {
		return &BlockStamp{
			Height: -1,
		}
	}

	lastSha := w.recent.hashes[nHashes-1]
	return &BlockStamp{
		Height: w.recent.lastHeight,
		Hash:   *lastSha,
	}
}

// NewIterateRecentBlocks returns an iterator for recently-seen blocks.
// The iterator starts at the most recently-added block, and Prev should
// be used to access earlier blocks.
func (w *Wallet) NewIterateRecentBlocks() RecentBlockIterator {
	return w.recent.NewIterator()
}

// EarliestBlockHeight returns the height of the blockchain for when any
// wallet address first appeared.  This will usually be the block height
// at the time of wallet creation, unless a private key with an earlier
// block height was imported into the wallet. This is needed when
// performing a full rescan to prevent unnecessary rescanning before
// wallet addresses first appeared.
func (w *Wallet) EarliestBlockHeight() int32 {
	height := w.keyGenerator.firstBlock

	// Imported keys will be the only ones that may have an earlier
	// blockchain height.  Check each and set the returned height
	for _, addr := range w.importedAddrs {
		if addr.firstBlock < height {
			height = addr.firstBlock

			// Can't go any lower than 0.
			if height == 0 {
				break
			}
		}
	}

	return height
}

// SetBetterEarliestBlockHeight sets a better earliest block height.
// At wallet creation time, a earliest block is guessed, but this
// could be incorrect if btcd is out of sync.  This function can be
// used to correct a previous guess with a better value.
func (w *Wallet) SetBetterEarliestBlockHeight(height int32) {
	if height > w.keyGenerator.firstBlock {
		w.keyGenerator.firstBlock = height
	}
}

// ImportPrivateKey creates a new encrypted btcAddress with a
// user-provided private key and adds it to the wallet.  If the
// import is successful, the payment address string is returned.
func (w *Wallet) ImportPrivateKey(privkey []byte, compressed bool, bs *BlockStamp) (string, error) {
	// First, must check that the key being imported will not result
	// in a duplicate address.
	pkh := btcutil.Hash160(pubkeyFromPrivkey(privkey, compressed))
	// Will always be valid inputs so omit error check.
	apkh, err := btcutil.NewAddressPubKeyHash(pkh, w.Net())
	if err != nil {
		return "", err
	}
	if _, ok := w.addrMap[*apkh]; ok {
		return "", ErrDuplicate
	}

	// The wallet's secret will be zeroed on lock, so make a local copy.
	w.secret.Lock()
	if len(w.secret.key) != 32 {
		w.secret.Unlock()
		return "", ErrWalletLocked
	}
	localSecret := make([]byte, 32)
	copy(localSecret, w.secret.key)
	w.secret.Unlock()

	// Create new address with this private key.
	btcaddr, err := newBtcAddress(privkey, nil, bs, compressed)
	if err != nil {
		return "", err
	}
	btcaddr.chainIndex = importedKeyChainIdx

	// Encrypt imported address with the derived AES key.
	if err = btcaddr.encrypt(localSecret); err != nil {
		return "", err
	}

	// Add address to wallet's bookkeeping structures.  Adding to
	// the map will result in the imported address being serialized
	// on the next WriteTo call.
	w.addrMap[*btcaddr.address(w.net)] = btcaddr
	w.importedAddrs = append(w.importedAddrs, btcaddr)

	// Create and return encoded payment address string.  Error is
	// ignored as the length of the pubkey hash and net will always
	// be valid.
	addr, _ := btcutil.NewAddressPubKeyHash(btcaddr.pubKeyHash[:], w.Net())
	return addr.String(), nil
}

// CreateDate returns the Unix time of the wallet creation time.  This
// is used to compare the wallet creation time against block headers and
// set a better minimum block height of where to being rescans.
func (w *Wallet) CreateDate() int64 {
	return w.createDate
}

// AddressInfo holds information regarding an address needed to manage
// a complete wallet.
type AddressInfo struct {
	btcutil.Address
	AddrHash   string
	Compressed bool
	FirstBlock int32
	Imported   bool
	Pubkey     string
}

// SortedActiveAddresses returns all wallet addresses that have been
// requested to be generated.  These do not include unused addresses in
// the key pool.  Use this when ordered addresses are needed.  Otherwise,
// ActiveAddresses is preferred.
func (w *Wallet) SortedActiveAddresses() []*AddressInfo {
	addrs := make([]*AddressInfo, 0,
		w.highestUsed+int64(len(w.importedAddrs))+1)
	for i := int64(rootKeyChainIdx); i <= w.highestUsed; i++ {
		a := w.chainIdxMap[i]
		info, err := w.addrMap[*a].info(w.Net())
		if err == nil {
			addrs = append(addrs, info)
		}
	}
	for _, addr := range w.importedAddrs {
		info, err := addr.info(w.Net())
		if err == nil {
			addrs = append(addrs, info)
		}
	}
	return addrs
}

// ActiveAddresses returns a map between active payment addresses
// and their full info.  These do not include unused addresses in the
// key pool.  If addresses must be sorted, use SortedActiveAddresses.
func (w *Wallet) ActiveAddresses() map[btcutil.Address]*AddressInfo {
	addrs := make(map[btcutil.Address]*AddressInfo)
	for i := int64(rootKeyChainIdx); i <= w.highestUsed; i++ {
		a := w.chainIdxMap[i]
		info, err := w.addrMap[*a].info(w.Net())
		if err == nil {
			addrs[info.Address] = info
		}
	}
	for _, addr := range w.importedAddrs {
		info, err := addr.info(w.Net())
		if err == nil {
			addrs[info.Address] = info
		}
	}
	return addrs
}

type walletFlags struct {
	useEncryption bool
	watchingOnly  bool
}

func (wf *walletFlags) ReadFrom(r io.Reader) (n int64, err error) {
	raw := make([]byte, 8)
	n, err = binaryRead(r, binary.LittleEndian, raw)
	wf.useEncryption = raw[0] != 0
	wf.watchingOnly = raw[1] != 0
	return n, err
}

func (wf *walletFlags) WriteTo(w io.Writer) (n int64, err error) {
	raw := make([]byte, 8)
	if wf.useEncryption {
		raw[0] = 1
	}
	if wf.watchingOnly {
		raw[1] = 1
	}
	return binaryWrite(w, binary.LittleEndian, raw)
}

type addrFlags struct {
	hasPrivKey              bool
	hasPubKey               bool
	encrypted               bool
	createPrivKeyNextUnlock bool // unimplemented in btcwallet
	compressed              bool
}

func (af *addrFlags) ReadFrom(r io.Reader) (n int64, err error) {
	var read int64
	var b [8]byte
	read, err = binaryRead(r, binary.LittleEndian, &b)
	if err != nil {
		return n + read, err
	}
	n += read

	if b[0]&(1<<0) != 0 {
		af.hasPrivKey = true
	}
	if b[0]&(1<<1) != 0 {
		af.hasPubKey = true
	}
	if b[0]&(1<<2) == 0 {
		return n, errors.New("address flag specifies unencrypted address")
	}
	af.encrypted = true
	if b[0]&(1<<3) != 0 {
		af.createPrivKeyNextUnlock = true
	}
	if b[0]&(1<<4) != 0 {
		af.compressed = true
	}

	return n, nil
}

func (af *addrFlags) WriteTo(w io.Writer) (n int64, err error) {
	var b [8]byte
	if af.hasPrivKey {
		b[0] |= 1 << 0
	}
	if af.hasPubKey {
		b[0] |= 1 << 1
	}
	if !af.encrypted {
		// We only support encrypted privkeys.
		return n, errors.New("address must be encrypted")
	}
	b[0] |= 1 << 2
	if af.createPrivKeyNextUnlock {
		b[0] |= 1 << 3
	}
	if af.compressed {
		b[0] |= 1 << 4
	}

	return binaryWrite(w, binary.LittleEndian, b)
}

// recentBlocks holds at most the last 20 seen block hashes as well as
// the block height of the most recently seen block.
type recentBlocks struct {
	hashes     []*btcwire.ShaHash
	lastHeight int32
}

type blockIterator struct {
	height int32
	index  int
	rb     *recentBlocks
}

func (rb *recentBlocks) ReadFromVersion(v version, r io.Reader) (int64, error) {
	if !v.LT(Vers20LastBlocks) {
		// Use current version.
		return rb.ReadFrom(r)
	}

	// Old file versions only saved the most recently seen
	// block height and hash, not the last 20.

	var read int64
	var syncedBlockHash btcwire.ShaHash

	// Read height.
	heightBytes := make([]byte, 4) // 4 bytes for a int32
	n, err := r.Read(heightBytes)
	if err != nil {
		return read + int64(n), err
	}
	read += int64(n)
	rb.lastHeight = int32(binary.LittleEndian.Uint32(heightBytes))

	// If height is -1, the last synced block is unknown, so don't try
	// to read a block hash.
	if rb.lastHeight == -1 {
		rb.hashes = nil
		return read, nil
	}

	// Read block hash.
	n, err = r.Read(syncedBlockHash[:])
	if err != nil {
		return read + int64(n), err
	}
	read += int64(n)

	rb.hashes = []*btcwire.ShaHash{
		&syncedBlockHash,
	}

	return read, nil
}

func (rb *recentBlocks) ReadFrom(r io.Reader) (int64, error) {
	var read int64

	// Read number of saved blocks.  This should not exceed 20.
	nBlockBytes := make([]byte, 4) // 4 bytes for a uint32
	n, err := r.Read(nBlockBytes)
	if err != nil {
		return read + int64(n), err
	}
	read += int64(n)
	nBlocks := binary.LittleEndian.Uint32(nBlockBytes)
	if nBlocks > 20 {
		return read, errors.New("number of last seen blocks exceeds maximum of 20")
	}

	// If number of blocks is 0, our work here is done.
	if nBlocks == 0 {
		rb.lastHeight = -1
		rb.hashes = nil
		return read, nil
	}

	// Read most recently seen block height.
	heightBytes := make([]byte, 4) // 4 bytes for a int32
	n, err = r.Read(heightBytes)
	if err != nil {
		return read + int64(n), err
	}
	read += int64(n)
	height := int32(binary.LittleEndian.Uint32(heightBytes))

	// height should not be -1 (or any other negative number)
	// since at this point we should be reading in at least one
	// known block.
	if height < 0 {
		return read, errors.New("expected a block but specified height is negative")
	}

	// Set last seen height.
	rb.lastHeight = height

	// Read nBlocks block hashes.  Hashes are expected to be in
	// order of oldest to newest, but there's no way to check
	// that here.
	rb.hashes = make([]*btcwire.ShaHash, 0, nBlocks)
	for i := uint32(0); i < nBlocks; i++ {
		blockSha := new(btcwire.ShaHash)
		n, err := r.Read(blockSha[:])
		if err != nil {
			return read + int64(n), err
		}
		read += int64(n)
		rb.hashes = append(rb.hashes, blockSha)
	}

	return read, nil
}

func (rb *recentBlocks) WriteTo(w io.Writer) (int64, error) {
	var written int64

	// Write number of saved blocks.  This should not exceed 20.
	nBlocks := uint32(len(rb.hashes))
	if nBlocks > 20 {
		return written, errors.New("number of last seen blocks exceeds maximum of 20")
	}
	if nBlocks != 0 && rb.lastHeight < 0 {
		return written, errors.New("number of block hashes is positive, but height is negative")
	}
	if nBlocks == 0 && rb.lastHeight != -1 {
		return written, errors.New("no block hashes available, but height is not -1")
	}
	nBlockBytes := make([]byte, 4) // 4 bytes for a uint32
	binary.LittleEndian.PutUint32(nBlockBytes, nBlocks)
	n, err := w.Write(nBlockBytes)
	if err != nil {
		return written + int64(n), err
	}
	written += int64(n)

	// If number of blocks is 0, our work here is done.
	if nBlocks == 0 {
		return written, nil
	}

	// Write most recently seen block height.
	heightBytes := make([]byte, 4) // 4 bytes for a int32
	binary.LittleEndian.PutUint32(heightBytes, uint32(rb.lastHeight))
	n, err = w.Write(heightBytes)
	if err != nil {
		return written + int64(n), err
	}
	written += int64(n)

	// Write block hashes.
	for _, hash := range rb.hashes {
		n, err := w.Write(hash[:])
		if err != nil {
			return written + int64(n), err
		}
		written += int64(n)
	}

	return written, nil
}

// RecentBlockIterator is a type to iterate through recent-seen
// blocks.
type RecentBlockIterator interface {
	Next() bool
	Prev() bool
	BlockStamp() *BlockStamp
}

func (rb *recentBlocks) NewIterator() RecentBlockIterator {
	if rb.lastHeight == -1 {
		return nil
	}
	return &blockIterator{
		height: rb.lastHeight,
		index:  len(rb.hashes) - 1,
		rb:     rb,
	}
}

func (it *blockIterator) Next() bool {
	if it.index+1 >= len(it.rb.hashes) {
		return false
	}
	it.index += 1
	return true
}

func (it *blockIterator) Prev() bool {
	if it.index-1 < 0 {
		return false
	}
	it.index -= 1
	return true
}

func (it *blockIterator) BlockStamp() *BlockStamp {
	return &BlockStamp{
		Height: it.rb.lastHeight - int32(len(it.rb.hashes)-1-it.index),
		Hash:   *it.rb.hashes[it.index],
	}
}

// unusedSpace is a wrapper type to read or write one or more types
// that btcwallet fits into an unused space left by Armory's wallet file
// format.
type unusedSpace struct {
	nBytes int // number of unused bytes that armory left.
	rfvs   []ReaderFromVersion
}

func newUnusedSpace(nBytes int, rfvs ...ReaderFromVersion) *unusedSpace {
	return &unusedSpace{
		nBytes: nBytes,
		rfvs:   rfvs,
	}
}

func (u *unusedSpace) ReadFromVersion(v version, r io.Reader) (int64, error) {
	var read int64

	for _, rfv := range u.rfvs {
		n, err := rfv.ReadFromVersion(v, r)
		if err != nil {
			return read + n, err
		}
		read += n
		if read > int64(u.nBytes) {
			return read, errors.New("read too much from armory's unused space")
		}
	}

	// Read rest of actually unused bytes.
	unused := make([]byte, u.nBytes-int(read))
	n, err := r.Read(unused)
	return read + int64(n), err
}

func (u *unusedSpace) WriteTo(w io.Writer) (int64, error) {
	var written int64

	for _, wt := range u.rfvs {
		n, err := wt.WriteTo(w)
		if err != nil {
			return written + n, err
		}
		written += n
		if written > int64(u.nBytes) {
			return written, errors.New("wrote too much to armory's unused space")
		}
	}

	// Write rest of actually unused bytes.
	unused := make([]byte, u.nBytes-int(written))
	n, err := w.Write(unused)
	return written + int64(n), err
}

type btcAddress struct {
	pubKeyHash [ripemd160.Size]byte
	flags      addrFlags
	chaincode  [32]byte
	chainIndex int64
	chainDepth int64 // currently unused (will use when extending a locked wallet)
	initVector [16]byte
	privKey    [32]byte
	pubKey     publicKey
	firstSeen  int64
	lastSeen   int64
	firstBlock int32
	lastBlock  int32
	privKeyCT  struct {
		sync.Mutex
		key []byte // non-nil if unlocked.
	}
}

const (
	// Root address has a chain index of -1. Each subsequent
	// chained address increments the index.
	rootKeyChainIdx = -1

	// Imported private keys are not part of the chain, and have a
	// special index of -2.
	importedKeyChainIdx = -2
)

const (
	pubkeyCompressed   byte = 0x2
	pubkeyUncompressed byte = 0x4
)

type publicKey []byte

func (k *publicKey) ReadFrom(r io.Reader) (n int64, err error) {
	var read int64
	var format byte
	read, err = binaryRead(r, binary.LittleEndian, &format)
	if err != nil {
		return n + read, err
	}
	n += read

	// Remove the oddness from the format
	noodd := format
	noodd &= ^byte(0x1)

	var s []byte
	switch noodd {
	case pubkeyUncompressed:
		// Read the remaining 64 bytes.
		s = make([]byte, 64)

	case pubkeyCompressed:
		// Read the remaining 32 bytes.
		s = make([]byte, 32)

	default:
		return n, errors.New("unrecognized pubkey format")
	}

	read, err = binaryRead(r, binary.LittleEndian, &s)
	if err != nil {
		return n + read, err
	}
	n += read

	*k = append([]byte{format}, s...)
	return
}

func (k *publicKey) WriteTo(w io.Writer) (n int64, err error) {
	return binaryWrite(w, binary.LittleEndian, []byte(*k))
}

// newBtcAddress initializes and returns a new address.  privkey must
// be 32 bytes.  iv must be 16 bytes, or nil (in which case it is
// randomly generated).
func newBtcAddress(privkey, iv []byte, bs *BlockStamp, compressed bool) (addr *btcAddress, err error) {
	if len(privkey) != 32 {
		return nil, errors.New("private key is not 32 bytes")
	}
	if iv == nil {
		iv = make([]byte, 16)
		if _, err := rand.Read(iv); err != nil {
			return nil, err
		}
	} else if len(iv) != 16 {
		return nil, errors.New("init vector must be nil or 16 bytes large")
	}

	addr = &btcAddress{
		flags: addrFlags{
			hasPrivKey: true,
			hasPubKey:  true,
			compressed: compressed,
		},
		firstSeen:  time.Now().Unix(),
		firstBlock: bs.Height,
	}
	addr.privKeyCT.key = privkey
	copy(addr.initVector[:], iv)
	addr.pubKey = pubkeyFromPrivkey(privkey, compressed)
	copy(addr.pubKeyHash[:], btcutil.Hash160(addr.pubKey))

	return addr, nil
}

// newRootBtcAddress generates a new address, also setting the
// chaincode and chain index to represent this address as a root
// address.
func newRootBtcAddress(privKey, iv, chaincode []byte,
	bs *BlockStamp) (addr *btcAddress, err error) {

	if len(chaincode) != 32 {
		return nil, errors.New("chaincode is not 32 bytes")
	}

	// Create new btcAddress with provided inputs.  This will
	// always use a compressed pubkey.
	addr, err = newBtcAddress(privKey, iv, bs, true)
	if err != nil {
		return nil, err
	}

	copy(addr.chaincode[:], chaincode)
	addr.chainIndex = rootKeyChainIdx

	return addr, err
}

// verifyKeypairs creates a signature using the parsed private key and
// verifies the signature with the parsed public key.  If either of these
// steps fail, the keypair generation failed and any funds sent to this
// address will be unspendable.  This step requires an unencrypted or
// unlocked btcAddress.
func (a *btcAddress) verifyKeypairs() error {
	// Parse public key.
	pubkey, err := btcec.ParsePubKey(a.pubKey, btcec.S256())
	if err != nil {
		return err
	}

	if len(a.privKeyCT.key) != 32 {
		return errors.New("private key unavailable")
	}

	privkey := &ecdsa.PrivateKey{
		PublicKey: *pubkey,
		D:         new(big.Int).SetBytes(a.privKeyCT.key),
	}

	data := "String to sign."
	r, s, err := ecdsa.Sign(rand.Reader, privkey, []byte(data))
	if err != nil {
		return err
	}

	ok := ecdsa.Verify(&privkey.PublicKey, []byte(data), r, s)
	if !ok {
		return errors.New("ecdsa verification failed")
	}
	return nil
}

// ReadFrom reads an encrypted address from an io.Reader.
func (a *btcAddress) ReadFrom(r io.Reader) (n int64, err error) {
	var read int64

	// Checksums
	var chkPubKeyHash uint32
	var chkChaincode uint32
	var chkInitVector uint32
	var chkPrivKey uint32
	var chkPubKey uint32

	// Read serialized wallet into addr fields and checksums.
	datas := []interface{}{
		&a.pubKeyHash,
		&chkPubKeyHash,
		make([]byte, 4), // version
		&a.flags,
		&a.chaincode,
		&chkChaincode,
		&a.chainIndex,
		&a.chainDepth,
		&a.initVector,
		&chkInitVector,
		&a.privKey,
		&chkPrivKey,
		&a.pubKey,
		&chkPubKey,
		&a.firstSeen,
		&a.lastSeen,
		&a.firstBlock,
		&a.lastBlock,
	}
	for _, data := range datas {
		if rf, ok := data.(io.ReaderFrom); ok {
			read, err = rf.ReadFrom(r)
		} else {
			read, err = binaryRead(r, binary.LittleEndian, data)
		}
		if err != nil {
			return n + read, err
		}
		n += read
	}

	// Verify checksums, correct errors where possible.
	checks := []struct {
		data []byte
		chk  uint32
	}{
		{a.pubKeyHash[:], chkPubKeyHash},
		{a.chaincode[:], chkChaincode},
		{a.initVector[:], chkInitVector},
		{a.privKey[:], chkPrivKey},
		{a.pubKey, chkPubKey},
	}
	for i := range checks {
		if err = verifyAndFix(checks[i].data, checks[i].chk); err != nil {
			return n, err
		}
	}

	return n, nil
}

func (a *btcAddress) WriteTo(w io.Writer) (n int64, err error) {
	var written int64

	datas := []interface{}{
		&a.pubKeyHash,
		walletHash(a.pubKeyHash[:]),
		make([]byte, 4), //version
		&a.flags,
		&a.chaincode,
		walletHash(a.chaincode[:]),
		&a.chainIndex,
		&a.chainDepth,
		&a.initVector,
		walletHash(a.initVector[:]),
		&a.privKey,
		walletHash(a.privKey[:]),
		&a.pubKey,
		walletHash(a.pubKey),
		&a.firstSeen,
		&a.lastSeen,
		&a.firstBlock,
		&a.lastBlock,
	}
	for _, data := range datas {
		if wt, ok := data.(io.WriterTo); ok {
			written, err = wt.WriteTo(w)
		} else {
			written, err = binaryWrite(w, binary.LittleEndian, data)
		}
		if err != nil {
			return n + written, err
		}
		n += written
	}
	return n, nil
}

// encrypt attempts to encrypt an address's clear text private key,
// failing if the address is already encrypted or if the private key is
// not 32 bytes.  If successful, the encryption flag is set.
func (a *btcAddress) encrypt(key []byte) error {
	if a.flags.encrypted {
		return errors.New("address already encrypted")
	}
	a.privKeyCT.Lock()
	defer a.privKeyCT.Unlock()
	if len(a.privKeyCT.key) != 32 {
		return errors.New("invalid clear text private key")
	}

	aesBlockEncrypter, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aesEncrypter := cipher.NewCFBEncrypter(aesBlockEncrypter, a.initVector[:])

	aesEncrypter.XORKeyStream(a.privKey[:], a.privKeyCT.key)

	a.flags.encrypted = true
	return nil
}

// lock removes the reference this address holds to its clear text
// private key.  This function fails if the address is not encrypted.
func (a *btcAddress) lock() error {
	if !a.flags.encrypted {
		return errors.New("unable to lock unencrypted address")
	}

	a.privKeyCT.Lock()
	zero(a.privKeyCT.key)
	a.privKeyCT.key = nil
	a.privKeyCT.Unlock()
	return nil
}

// unlock decrypts and stores a pointer to this address's private key,
// failing if the address is not encrypted, or the provided key is
// incorrect.  The returned clear text private key will always be a copy
// that may be safely used by the caller without worrying about it being
// zeroed during an address lock.
func (a *btcAddress) unlock(key []byte) (privKeyCT []byte, err error) {
	if !a.flags.encrypted {
		return nil, errors.New("unable to unlock unencrypted address")
	}

	// If secret is already saved, return a copy without performing a full
	// unlock.
	a.privKeyCT.Lock()
	if len(a.privKeyCT.key) == 32 {
		privKeyCT := make([]byte, 32)
		copy(privKeyCT, a.privKeyCT.key)
		a.privKeyCT.Unlock()
		return privKeyCT, nil
	}
	a.privKeyCT.Unlock()

	// Decrypt private key with AES key.
	aesBlockDecrypter, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aesDecrypter := cipher.NewCFBDecrypter(aesBlockDecrypter, a.initVector[:])
	privkey := make([]byte, 32)
	aesDecrypter.XORKeyStream(privkey, a.privKey[:])

	// Generate new x, y from clear text private key and check that they
	// match the recorded pubkey.
	pubKey, err := btcec.ParsePubKey(a.pubKey, btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("cannot parse pubkey: %s", err)
	}
	x, y := btcec.S256().ScalarBaseMult(privkey)
	if x.Cmp(pubKey.X) != 0 || y.Cmp(pubKey.Y) != 0 {
		return nil, errors.New("decryption failed")
	}

	privkeyCopy := make([]byte, 32)
	copy(privkeyCopy, privkey)
	a.privKeyCT.Lock()
	a.privKeyCT.key = privkey
	a.privKeyCT.Unlock()
	return privkeyCopy, nil
}

// TODO(jrick)
func (a *btcAddress) changeEncryptionKey(oldkey, newkey []byte) error {
	return errors.New("unimplemented")
}

// address returns a btcutil.AddressPubKeyHash for a btcAddress.
func (a *btcAddress) address(net btcwire.BitcoinNet) *btcutil.AddressPubKeyHash {
	// error is not returned because the hash will always be 20
	// bytes, and net is assumed to be valid.
	addr, _ := btcutil.NewAddressPubKeyHash(a.pubKeyHash[:], net)
	return addr
}

// info returns information about a btcAddress stored in a AddressInfo
// struct.
func (a *btcAddress) info(net btcwire.BitcoinNet) (*AddressInfo, error) {
	address := a.address(net)

	return &AddressInfo{
		Address:    address,
		AddrHash:   string(a.pubKeyHash[:]),
		Compressed: a.flags.compressed,
		FirstBlock: a.firstBlock,
		Imported:   a.chainIndex == importedKeyChainIdx,
		Pubkey:     hex.EncodeToString(a.pubKey),
	}, nil
}

func walletHash(b []byte) uint32 {
	sum := btcwire.DoubleSha256(b)
	return binary.LittleEndian.Uint32(sum)
}

// TODO(jrick) add error correction.
func verifyAndFix(b []byte, chk uint32) error {
	if walletHash(b) != chk {
		return ErrChecksumMismatch
	}
	return nil
}

type kdfParameters struct {
	mem   uint64
	nIter uint32
	salt  [32]byte
}

// computeKdfParameters returns best guess parameters to the
// memory-hard key derivation function to make the computation last
// targetSec seconds, while using no more than maxMem bytes of memory.
func computeKdfParameters(targetSec float64, maxMem uint64) (*kdfParameters, error) {
	params := &kdfParameters{}
	if _, err := rand.Read(params.salt[:]); err != nil {
		return nil, err
	}

	testKey := []byte("This is an example key to test KDF iteration speed")

	memoryReqtBytes := uint64(1024)
	approxSec := float64(0)

	for approxSec <= targetSec/4 && memoryReqtBytes < maxMem {
		memoryReqtBytes *= 2
		before := time.Now()
		_ = keyOneIter(testKey, params.salt[:], memoryReqtBytes)
		approxSec = time.Since(before).Seconds()
	}

	allItersSec := float64(0)
	nIter := uint32(1)
	for allItersSec < 0.02 { // This is a magic number straight from armory's source.
		nIter *= 2
		before := time.Now()
		for i := uint32(0); i < nIter; i++ {
			_ = keyOneIter(testKey, params.salt[:], memoryReqtBytes)
		}
		allItersSec = time.Since(before).Seconds()
	}

	params.mem = memoryReqtBytes
	params.nIter = nIter

	return params, nil
}

func (params *kdfParameters) WriteTo(w io.Writer) (n int64, err error) {
	var written int64

	memBytes := make([]byte, 8)
	nIterBytes := make([]byte, 4)
	binary.LittleEndian.PutUint64(memBytes, params.mem)
	binary.LittleEndian.PutUint32(nIterBytes, params.nIter)
	chkedBytes := append(memBytes, nIterBytes...)
	chkedBytes = append(chkedBytes, params.salt[:]...)

	datas := []interface{}{
		&params.mem,
		&params.nIter,
		&params.salt,
		walletHash(chkedBytes),
		make([]byte, 256-(binary.Size(params)+4)), // padding
	}
	for _, data := range datas {
		if written, err = binaryWrite(w, binary.LittleEndian, data); err != nil {
			return n + written, err
		}
		n += written
	}

	return n, nil
}

func (params *kdfParameters) ReadFrom(r io.Reader) (n int64, err error) {
	var read int64

	// These must be read in but are not saved directly to params.
	chkedBytes := make([]byte, 44)
	var chk uint32
	padding := make([]byte, 256-(binary.Size(params)+4))

	datas := []interface{}{
		chkedBytes,
		&chk,
		padding,
	}
	for _, data := range datas {
		if read, err = binaryRead(r, binary.LittleEndian, data); err != nil {
			return n + read, err
		}
		n += read
	}

	// Verify checksum
	if err = verifyAndFix(chkedBytes, chk); err != nil {
		return n, err
	}

	// Read params
	buf := bytes.NewBuffer(chkedBytes)
	datas = []interface{}{
		&params.mem,
		&params.nIter,
		&params.salt,
	}
	for _, data := range datas {
		if err = binary.Read(buf, binary.LittleEndian, data); err != nil {
			return n, err
		}
	}

	return n, nil
}

type addrEntry struct {
	pubKeyHash160 [ripemd160.Size]byte
	addr          btcAddress
}

func (e *addrEntry) WriteTo(w io.Writer) (n int64, err error) {
	var written int64

	// Write header
	if written, err = binaryWrite(w, binary.LittleEndian, addrHeader); err != nil {
		return n + written, err
	}
	n += written

	// Write hash
	if written, err = binaryWrite(w, binary.LittleEndian, &e.pubKeyHash160); err != nil {
		return n + written, err
	}
	n += written

	// Write btcAddress
	written, err = e.addr.WriteTo(w)
	n += written
	return n, err
}

func (e *addrEntry) ReadFrom(r io.Reader) (n int64, err error) {
	var read int64

	if read, err = binaryRead(r, binary.LittleEndian, &e.pubKeyHash160); err != nil {
		return n + read, err
	}
	n += read

	read, err = e.addr.ReadFrom(r)
	return n + read, err
}

type addrCommentEntry struct {
	pubKeyHash160 [ripemd160.Size]byte
	comment       []byte
}

func (e *addrCommentEntry) address(net btcwire.BitcoinNet) *btcutil.AddressPubKeyHash {
	// error is not returned because the hash will always be 20
	// bytes, and net is assumed to be valid.
	addr, _ := btcutil.NewAddressPubKeyHash(e.pubKeyHash160[:], net)
	return addr
}

func (e *addrCommentEntry) WriteTo(w io.Writer) (n int64, err error) {
	var written int64

	// Comments shall not overflow their entry.
	if len(e.comment) > maxCommentLen {
		return n, ErrMalformedEntry
	}

	// Write header
	if written, err = binaryWrite(w, binary.LittleEndian, addrCommentHeader); err != nil {
		return n + written, err
	}
	n += written

	// Write hash
	if written, err = binaryWrite(w, binary.LittleEndian, &e.pubKeyHash160); err != nil {
		return n + written, err
	}
	n += written

	// Write length
	if written, err = binaryWrite(w, binary.LittleEndian, uint16(len(e.comment))); err != nil {
		return n + written, err
	}
	n += written

	// Write comment
	written, err = binaryWrite(w, binary.LittleEndian, e.comment)
	return n + written, err
}

func (e *addrCommentEntry) ReadFrom(r io.Reader) (n int64, err error) {
	var read int64

	if read, err = binaryRead(r, binary.LittleEndian, &e.pubKeyHash160); err != nil {
		return n + read, err
	}
	n += read

	var clen uint16
	if read, err = binaryRead(r, binary.LittleEndian, &clen); err != nil {
		return n + read, err
	}
	n += read

	e.comment = make([]byte, clen)
	read, err = binaryRead(r, binary.LittleEndian, e.comment)
	return n + read, err
}

type txCommentEntry struct {
	txHash  [sha256.Size]byte
	comment []byte
}

func (e *txCommentEntry) WriteTo(w io.Writer) (n int64, err error) {
	var written int64

	// Comments shall not overflow their entry.
	if len(e.comment) > maxCommentLen {
		return n, ErrMalformedEntry
	}

	// Write header
	if written, err = binaryWrite(w, binary.LittleEndian, txCommentHeader); err != nil {
		return n + written, err
	}
	n += written

	// Write length
	if written, err = binaryWrite(w, binary.LittleEndian, uint16(len(e.comment))); err != nil {
		return n + written, err
	}

	// Write comment
	written, err = binaryWrite(w, binary.LittleEndian, e.comment)
	return n + written, err
}

func (e *txCommentEntry) ReadFrom(r io.Reader) (n int64, err error) {
	var read int64

	if read, err = binaryRead(r, binary.LittleEndian, &e.txHash); err != nil {
		return n + read, err
	}
	n += read

	var clen uint16
	if read, err = binaryRead(r, binary.LittleEndian, &clen); err != nil {
		return n + read, err
	}
	n += read

	e.comment = make([]byte, clen)
	read, err = binaryRead(r, binary.LittleEndian, e.comment)
	return n + read, err
}

type deletedEntry struct{}

func (e *deletedEntry) ReadFrom(r io.Reader) (n int64, err error) {
	var read int64

	var ulen uint16
	if read, err = binaryRead(r, binary.LittleEndian, &ulen); err != nil {
		return n + read, err
	}
	n += read

	unused := make([]byte, ulen)
	nRead, err := r.Read(unused)
	if err == io.EOF {
		return n + int64(nRead), nil
	}
	return n + int64(nRead), err
}

// BlockStamp defines a block (by height and a unique hash) and is
// used to mark a point in the blockchain that a wallet element is
// synced to.
type BlockStamp struct {
	Height int32
	Hash   btcwire.ShaHash
}
