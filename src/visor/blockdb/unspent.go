package blockdb

import (
	"fmt"
	"sync"

	"github.com/boltdb/bolt"
	"github.com/skycoin/skycoin/src/cipher"
	"github.com/skycoin/skycoin/src/cipher/encoder"
	"github.com/skycoin/skycoin/src/coin"
	"github.com/skycoin/skycoin/src/visor/bucket"
)

var (
	xorhashKey = []byte("xorhash")
)

// UnspentGetter provides unspend pool related
// querying methods
type UnspentGetter interface {
	// GetUnspentsOfAddrs returns all unspent outputs of given addresses
	GetUnspentsOfAddrs(addrs []cipher.Address) coin.AddressUxOuts
	Get(cipher.SHA256) (coin.UxOut, bool)
}

// UnspentPool unspent outputs pool
type UnspentPool struct {
	db    *bolt.DB
	pool  *bucket.Bucket
	meta  *bucket.Bucket
	cache struct {
		pool   map[string]coin.UxOut
		uxhash cipher.SHA256
	}
	sync.Mutex
}

type unspentMeta struct {
	*bolt.Bucket
}

func (m unspentMeta) getXorHash() (cipher.SHA256, error) {
	if v := m.Get(xorhashKey); v != nil {
		var hash cipher.SHA256
		copy(hash[:], v[:])
		return hash, nil
	}

	return cipher.SHA256{}, nil
}

func (m *unspentMeta) setXorHash(hash cipher.SHA256) error {
	return m.Put(xorhashKey, hash[:])
}

type uxOuts struct {
	*bolt.Bucket
}

func (uo uxOuts) get(hash cipher.SHA256) (*coin.UxOut, bool, error) {
	if v := uo.Get(hash[:]); v != nil {
		var out coin.UxOut
		if err := encoder.DeserializeRaw(v, &out); err != nil {
			return nil, false, err
		}
		return &out, true, nil
	}
	return nil, false, nil
}

func (uo uxOuts) set(hash cipher.SHA256, ux coin.UxOut) error {
	v := encoder.Serialize(ux)
	return uo.Put(hash[:], v)
}

func (uo *uxOuts) delete(hash cipher.SHA256) error {
	return uo.Delete(hash[:])
}

// NewUnspentPool creates new unspent pool instance
func NewUnspentPool(db *bolt.DB) (*UnspentPool, error) {
	up := &UnspentPool{db: db}
	up.cache.pool = make(map[string]coin.UxOut)

	pool, err := bucket.New([]byte("unspent_pool"), db)
	if err != nil {
		return nil, err
	}
	up.pool = pool

	meta, err := bucket.New([]byte("unspent_meta"), db)
	if err != nil {
		return nil, err
	}
	up.meta = meta

	// load from db
	if err := up.syncCache(); err != nil {
		return nil, err
	}

	return up, nil
}

func (up *UnspentPool) syncCache() error {
	// load unspent outputs
	if err := up.pool.ForEach(func(k, v []byte) error {
		var hash cipher.SHA256
		copy(hash[:], k[:])

		var ux coin.UxOut
		if err := encoder.DeserializeRaw(v, &ux); err != nil {
			return fmt.Errorf("load unspent outputs from db failed: %v", err)
		}

		up.cache.pool[hash.Hex()] = ux
		return nil
	}); err != nil {
		return err
	}

	// load uxhash
	uxhash, err := up.getUxHashFromDB()
	if err != nil {
		return err
	}

	up.cache.uxhash = uxhash
	return nil
}

func (up *UnspentPool) processBlock(b *coin.Block) bucket.TxHandler {
	return func(tx *bolt.Tx) (bucket.Rollback, error) {
		var (
			delUxs    []coin.UxOut
			addUxs    []coin.UxOut
			uxHash    cipher.SHA256
			oldUxHash = up.cache.uxhash
		)

		for _, txn := range b.Body.Transactions {
			// get uxouts need to be deleted
			uxs, err := up.getArray(txn.In)
			if err != nil {
				return func() {}, err
			}

			delUxs = append(delUxs, uxs...)

			// Remove spent outputs
			if _, err = up.deleteWithTx(tx, txn.In); err != nil {
				return func() {}, err
			}

			// Create new outputs
			txUxs := coin.CreateUnspents(b.Head, txn)
			addUxs = append(addUxs, txUxs...)
			for i := range txUxs {
				uxHash, err = up.addWithTx(tx, txUxs[i])
				if err != nil {
					return func() {}, err
				}
			}
		}

		// update caches
		up.Lock()
		up.deleteUxFromCache(delUxs)
		up.addUxToCache(addUxs)
		up.updateUxHashInCache(uxHash)
		up.Unlock()

		return func() {
			up.Lock()
			// reverse the cache
			up.deleteUxFromCache(addUxs)
			up.addUxToCache(delUxs)
			up.updateUxHashInCache(oldUxHash)
			up.Unlock()
		}, nil
	}
}

func (up *UnspentPool) addWithTx(tx *bolt.Tx, ux coin.UxOut) (uxhash cipher.SHA256, err error) {
	// will rollback all updates if return is not nil
	// in case of unexpected panic, we must catch it and return error
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("unspent pool add uxout failed: %v", err)
		}
	}()

	// check if the uxout does exist in the pool
	h := ux.Hash()
	if up.Contains(h) {
		return cipher.SHA256{}, fmt.Errorf("attemps to insert uxout:%v twice into the unspent pool", h.Hex())
	}

	meta := unspentMeta{tx.Bucket(up.meta.Name)}
	xorhash, err := meta.getXorHash()
	if err != nil {
		return cipher.SHA256{}, err
	}

	xorhash = xorhash.Xor(ux.SnapshotHash())
	if err := meta.setXorHash(xorhash); err != nil {
		return cipher.SHA256{}, err
	}

	err = uxOuts{tx.Bucket(up.pool.Name)}.set(h, ux)
	if err != nil {
		return cipher.SHA256{}, err
	}

	return xorhash, nil
}

func (up *UnspentPool) deleteUxFromCache(uxs []coin.UxOut) {
	for _, ux := range uxs {
		delete(up.cache.pool, ux.Hash().Hex())
	}
}

func (up *UnspentPool) addUxToCache(uxs []coin.UxOut) {
	for i, ux := range uxs {
		up.cache.pool[ux.Hash().Hex()] = uxs[i]
	}
}

func (up *UnspentPool) updateUxHashInCache(hash cipher.SHA256) {
	up.cache.uxhash = hash
}

// GetArray returns UxOut by given hash array, will return error when
// if any of the hashes is not exist.
func (up *UnspentPool) GetArray(hashes []cipher.SHA256) (coin.UxArray, error) {
	up.Lock()
	defer up.Unlock()
	return up.getArray(hashes)
}

func (up *UnspentPool) getArray(hashes []cipher.SHA256) (coin.UxArray, error) {
	uxs := make(coin.UxArray, 0, len(hashes))
	for i := range hashes {
		ux, ok := up.cache.pool[hashes[i].Hex()]
		if !ok {
			return nil, fmt.Errorf("unspent output of %s does not exist", hashes[i].Hex())
		}

		uxs = append(uxs, ux)
	}
	return uxs, nil
}

// Get returns the uxout value of given hash
func (up *UnspentPool) Get(h cipher.SHA256) (coin.UxOut, bool) {
	up.Lock()
	ux, ok := up.cache.pool[h.Hex()]
	up.Unlock()

	return ux, ok
}

// GetAll returns Pool as an array. Note: they are not in any particular order.
func (up *UnspentPool) GetAll() (coin.UxArray, error) {
	up.Lock()
	arr := make(coin.UxArray, 0, len(up.cache.pool))
	for _, ux := range up.cache.pool {
		arr = append(arr, ux)
	}
	up.Unlock()

	return arr, nil
}

// delete delete unspent of given hashes
func (up *UnspentPool) deleteWithTx(tx *bolt.Tx, hashes []cipher.SHA256) (cipher.SHA256, error) {
	uxouts := uxOuts{tx.Bucket(up.pool.Name)}
	meta := unspentMeta{tx.Bucket(up.meta.Name)}
	var uxHash cipher.SHA256
	for _, hash := range hashes {
		ux, ok, err := uxouts.get(hash)
		if err != nil {
			return cipher.SHA256{}, err
		}

		if !ok {
			continue
		}

		uxHash, err = meta.getXorHash()
		if err != nil {
			return cipher.SHA256{}, err
		}

		uxHash = uxHash.Xor(ux.SnapshotHash())

		// update uxhash
		if err = meta.setXorHash(uxHash); err != nil {
			return cipher.SHA256{}, err
		}

		if err := uxouts.delete(hash); err != nil {
			return cipher.SHA256{}, err
		}
	}

	return uxHash, nil
}

// Len returns the unspent outputs num
func (up *UnspentPool) Len() uint64 {
	up.Lock()
	defer up.Unlock()
	return uint64(len(up.cache.pool))
}

// Collides checks for hash collisions with existing hashes
func (up *UnspentPool) Collides(hashes []cipher.SHA256) bool {
	up.Lock()
	for i := range hashes {
		if _, ok := up.cache.pool[hashes[i].Hex()]; ok {
			return true
		}
	}
	up.Unlock()
	return false
}

// Contains check if the hash of uxout does exist in the pool
func (up *UnspentPool) Contains(h cipher.SHA256) bool {
	up.Lock()
	_, ok := up.cache.pool[h.Hex()]
	defer up.Unlock()
	return ok
}

// GetUnspentsOfAddr returns all unspent outputs of given address
func (up *UnspentPool) GetUnspentsOfAddr(addr cipher.Address) coin.UxArray {
	up.Lock()
	uxs := make(coin.UxArray, 0, len(up.cache.pool))
	for _, ux := range up.cache.pool {
		if ux.Body.Address == addr {
			uxs = append(uxs, ux)
		}
	}
	up.Unlock()
	return uxs
}

// GetUnspentsOfAddrs returns unspent outputs map of given addresses,
// the address as return map key, unspent outputs as value.
func (up *UnspentPool) GetUnspentsOfAddrs(addrs []cipher.Address) coin.AddressUxOuts {
	up.Lock()
	addrm := make(map[cipher.Address]struct{}, len(addrs))
	for _, a := range addrs {
		addrm[a] = struct{}{}
	}

	addrUxs := coin.AddressUxOuts{}
	for _, ux := range up.cache.pool {
		if _, ok := addrm[ux.Body.Address]; ok {
			addrUxs[ux.Body.Address] = append(addrUxs[ux.Body.Address], ux)
		}
	}
	up.Unlock()
	return addrUxs
}

// GetUxHash returns unspent output checksum for the Block.
// Must be called after Block is fully initialized,
// and before its outputs are added to the unspent pool
func (up *UnspentPool) GetUxHash() cipher.SHA256 {
	up.Lock()
	defer up.Unlock()
	return up.cache.uxhash
}

func (up *UnspentPool) getUxHashFromDB() (cipher.SHA256, error) {
	if v := up.meta.Get(xorhashKey); v != nil {
		var hash cipher.SHA256
		copy(hash[:], v[:])
		return hash, nil
	}
	return cipher.SHA256{}, nil
}
