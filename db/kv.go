// Copyright (c) 2013 Uwe Hoffmann. All rights reserved.

/*
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
*/

package db

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"path/filepath"

	"github.com/uwedeportivo/romba/types"

	"github.com/golang/glog"
)

const (
	datsDBName    = "dats_db"
	crcDBName     = "crc_db"
	md5DBName     = "md5_db"
	sha1DBName    = "sha1_db"
	crcsha1DBName = "crcsha1_db"
	md5sha1DBName = "md5sha1_db"
)

const (
	numParts    = 51
	keySizeCrc  = 4
	keySizeMd5  = 16
	keySizeSha1 = 20
)

type KVStore interface {
	Append(key, value []byte) error
	Set(key, value []byte) error
	Delete(key []byte) error
	Get(key []byte) ([]byte, error)
	Exists(key []byte) (bool, error)
	Flush()
	Size() int64
	StartBatch() KVBatch
	WriteBatch(batch KVBatch) error
	Close() error
	BeginRefresh() error
	EndRefresh() error
	PrintStats() string
}

type KVBatch interface {
	Set(key, value []byte) error
	Append(key, value []byte) error
	Delete(key []byte) error
	Clear()
}

var StoreOpener func(pathPrefix string, keySize int) (KVStore, error)

type kvStore struct {
	generation int64
	datsDB     KVStore
	crcDB      KVStore
	md5DB      KVStore
	sha1DB     KVStore
	crcsha1DB  KVStore
	md5sha1DB  KVStore
	path       string
}

type kvBatch struct {
	db           *kvStore
	datsBatch    KVBatch
	crcBatch     KVBatch
	md5Batch     KVBatch
	sha1Batch    KVBatch
	crcsha1Batch KVBatch
	md5sha1Batch KVBatch
	size         int64
}

func openDb(pathPrefix string, keySize int) (KVStore, error) {
	return StoreOpener(pathPrefix, keySize)
}

func NewKVStoreDB(path string) (RomDB, error) {
	kvdb := new(kvStore)
	kvdb.path = path

	glog.Infof("Loading Generation File")
	gen, err := ReadGenerationFile(path)
	if err != nil {
		return nil, err
	}
	kvdb.generation = gen

	glog.Infof("Loading Dats DB")
	db, err := openDb(filepath.Join(path, datsDBName), keySizeSha1)
	if err != nil {
		return nil, err
	}
	kvdb.datsDB = db

	glog.Infof("Loading CRC DB")
	db, err = openDb(filepath.Join(path, crcDBName), keySizeCrc)
	if err != nil {
		return nil, err
	}
	kvdb.crcDB = db

	glog.Infof("Loading MD5 DB")
	db, err = openDb(filepath.Join(path, md5DBName), keySizeMd5)
	if err != nil {
		return nil, err
	}
	kvdb.md5DB = db

	glog.Infof("Loading SHA1 DB")
	db, err = openDb(filepath.Join(path, sha1DBName), keySizeSha1)
	if err != nil {
		return nil, err
	}
	kvdb.sha1DB = db

	glog.Infof("Loading CRC -> SHA1 DB")
	db, err = openDb(filepath.Join(path, crcsha1DBName), keySizeCrc)
	if err != nil {
		return nil, err
	}
	kvdb.crcsha1DB = db

	glog.Infof("Loading MD5 -> SHA1 DB")
	db, err = openDb(filepath.Join(path, md5sha1DBName), keySizeMd5)
	if err != nil {
		return nil, err
	}
	kvdb.md5sha1DB = db

	return kvdb, nil
}

func init() {
	DBFactory = NewKVStoreDB
}

func (kvdb *kvStore) IndexRom(rom *types.Rom) error {
	batch := kvdb.StartBatch()
	err := batch.IndexRom(rom)
	if err != nil {
		return err
	}
	return batch.Close()
}

func (kvdb *kvStore) IndexDat(dat *types.Dat, sha1Bytes []byte) error {
	batch := kvdb.StartBatch()
	err := batch.IndexDat(dat, sha1Bytes)
	if err != nil {
		return err
	}
	return batch.Close()
}

func (kvdb *kvStore) OrphanDats() error {
	kvdb.generation++
	err := WriteGenerationFile(kvdb.path, kvdb.generation)
	if err != nil {
		return err
	}
	return nil
}

func (kvdb *kvStore) Generation() int64 {
	return kvdb.generation
}

func (kvdb *kvStore) GetDat(sha1Bytes []byte) (*types.Dat, error) {
	dBytes, err := kvdb.datsDB.Get(sha1Bytes)
	if err != nil {
		return nil, err
	}

	if dBytes == nil {
		return nil, nil
	}
	buf := bytes.NewBuffer(dBytes)
	datDecoder := gob.NewDecoder(buf)

	var dat types.Dat

	err = datDecoder.Decode(&dat)
	if err != nil {
		return nil, err
	}
	return &dat, nil
}

func (kvdb *kvStore) DatsForRom(rom *types.Rom) ([]*types.Dat, error) {
	var dBytes []byte
	var err error

	if rom.Sha1 != nil {
		dBytes, err = kvdb.sha1DB.Get(rom.Sha1)
		if err != nil {
			return nil, err
		}
	}
	if rom.Md5 != nil && dBytes == nil {
		dBytes, err = kvdb.md5DB.Get(rom.Md5)
		if err != nil {
			return nil, err
		}
	}
	if rom.Crc != nil && dBytes == nil {
		dBytes, err = kvdb.crcDB.Get(rom.Crc)
		if err != nil {
			return nil, err
		}
	}

	if dBytes == nil {
		return nil, nil
	}

	var dats []*types.Dat

	for i := 0; i < len(dBytes); i += sha1.Size {
		sha1Bytes := dBytes[i : i+sha1.Size]

		dat, err := kvdb.GetDat(sha1Bytes)
		if err != nil {
			return nil, err
		}
		if dat != nil {
			dats = append(dats, dat)
		}
	}

	return dats, nil
}

func (kvdb *kvStore) CompleteRom(rom *types.Rom) error {
	if rom.Sha1 != nil {
		return nil
	}

	if rom.Md5 != nil {
		dBytes, err := kvdb.md5sha1DB.Get(rom.Md5)
		if err != nil {
			return err
		}
		if len(dBytes) >= sha1.Size {
			rom.Sha1 = dBytes[:sha1.Size]
		}
		return nil
	}

	if rom.Crc != nil {
		dBytes, err := kvdb.crcsha1DB.Get(rom.Crc)
		if err != nil {
			return err
		}
		if len(dBytes) >= sha1.Size {
			rom.Sha1 = dBytes[:sha1.Size]
		}
	}
	return nil
}

func (kvdb *kvStore) Flush() {
	kvdb.datsDB.Flush()
	kvdb.crcDB.Flush()
	kvdb.md5DB.Flush()
	kvdb.sha1DB.Flush()
	kvdb.crcsha1DB.Flush()
	kvdb.md5sha1DB.Flush()
}

func (kvdb *kvStore) Close() error {
	kvdb.Flush()

	err := kvdb.datsDB.Close()
	if err != nil {
		return err
	}

	err = kvdb.crcDB.Close()
	if err != nil {
		return err
	}

	err = kvdb.md5DB.Close()
	if err != nil {
		return err
	}

	err = kvdb.sha1DB.Close()
	if err != nil {
		return err
	}

	err = kvdb.crcsha1DB.Close()
	if err != nil {
		return err
	}

	err = kvdb.md5sha1DB.Close()
	if err != nil {
		return err
	}
	return nil
}

func (kvdb *kvStore) BeginDatRefresh() error {
	return kvdb.datsDB.BeginRefresh()
}

func (kvdb *kvStore) PrintStats() string {
	buf := new(bytes.Buffer)

	fmt.Fprintf(buf, "\ndatsDB stats: %s\n", kvdb.datsDB.PrintStats())
	fmt.Fprintf(buf, "crcDB stats: %s\n", kvdb.crcDB.PrintStats())
	fmt.Fprintf(buf, "md5DB stats: %s\n", kvdb.md5DB.PrintStats())
	fmt.Fprintf(buf, "sha1DB stats: %s\n", kvdb.sha1DB.PrintStats())
	fmt.Fprintf(buf, "crcsha1DB stats: %s\n", kvdb.crcsha1DB.PrintStats())
	fmt.Fprintf(buf, "md5sha1DB stats: %s\n", kvdb.md5sha1DB.PrintStats())

	return buf.String()
}

func (kvdb *kvStore) EndDatRefresh() error {
	return kvdb.datsDB.EndRefresh()
}

func (kvdb *kvStore) StartBatch() RomBatch {
	return &kvBatch{
		db:           kvdb,
		datsBatch:    kvdb.datsDB.StartBatch(),
		crcBatch:     kvdb.crcDB.StartBatch(),
		md5Batch:     kvdb.md5DB.StartBatch(),
		sha1Batch:    kvdb.sha1DB.StartBatch(),
		crcsha1Batch: kvdb.crcsha1DB.StartBatch(),
		md5sha1Batch: kvdb.md5sha1DB.StartBatch(),
	}
}

func (kvb *kvBatch) Flush() error {
	if kvb.size == 0 {
		return nil
	}

	err := kvb.db.datsDB.WriteBatch(kvb.datsBatch)
	if err != nil {
		return err
	}
	kvb.datsBatch.Clear()

	err = kvb.db.crcDB.WriteBatch(kvb.crcBatch)
	if err != nil {
		return err
	}
	kvb.crcBatch.Clear()

	err = kvb.db.md5DB.WriteBatch(kvb.md5Batch)
	if err != nil {
		return err
	}
	kvb.md5Batch.Clear()

	err = kvb.db.sha1DB.WriteBatch(kvb.sha1Batch)
	if err != nil {
		return err
	}
	kvb.sha1Batch.Clear()

	err = kvb.db.crcsha1DB.WriteBatch(kvb.crcsha1Batch)
	if err != nil {
		return err
	}
	kvb.crcsha1Batch.Clear()

	err = kvb.db.md5sha1DB.WriteBatch(kvb.md5sha1Batch)
	if err != nil {
		return err
	}
	kvb.md5sha1Batch.Clear()

	kvb.size = 0
	return nil
}

func (kvb *kvBatch) Close() error {
	err := kvb.Flush()
	kvb.db = nil
	return err
}

func appendUniqueSha1(dst, src []byte) []byte {
	for i := 0; i < len(src); i += sha1.Size {
		srcBytes := src[i : i+sha1.Size]
		found := false
		for j := 0; j < len(dst); j += sha1.Size {
			dstBytes := dst[j : j+sha1.Size]
			if bytes.Equal(srcBytes, dstBytes) {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, srcBytes...)
		}
	}
	return dst
}

func (kvb *kvBatch) IndexRom(rom *types.Rom) error {
	glog.V(4).Infof("indexing rom %s", rom.Name)

	if rom.Sha1 != nil {
		if rom.Crc != nil {
			glog.V(4).Infof("declaring crc %s -> sha1 %s mapping", hex.EncodeToString(rom.Crc), hex.EncodeToString(rom.Sha1))
			err := kvb.crcsha1Batch.Append(rom.Crc, rom.Sha1)
			if err != nil {
				return err
			}
			kvb.size += int64(sha1.Size)
		}
		if rom.Md5 != nil {
			glog.V(4).Infof("declaring md5 %s -> sha1 %s mapping", hex.EncodeToString(rom.Md5), hex.EncodeToString(rom.Sha1))
			err := kvb.md5sha1Batch.Append(rom.Md5, rom.Sha1)
			if err != nil {
				return err
			}
			kvb.size += int64(sha1.Size)
		}
	} else {
		glog.Warningf("indexing rom %s with missing SHA1", rom.Name)
	}

	dats, err := kvb.db.DatsForRom(rom)
	if err != nil {
		return err
	}

	if len(dats) > 0 {
		glog.V(4).Infof("rom %s in at least dat %s", rom.Name, dats[0].Path)

		ssd, err := kvb.db.sha1DB.Get(rom.Sha1)
		if err != nil {
			return err
		}

		if len(ssd) == 0 {
			var sha1s []byte

			if rom.Md5 != nil {
				ss, err := kvb.db.md5DB.Get(rom.Md5)
				if err != nil {
					return err
				}
				if len(ss) > 0 {
					sha1s = appendUniqueSha1(sha1s, ss)
				}
			}
			if rom.Crc != nil {
				ss, err := kvb.db.crcDB.Get(rom.Crc)
				if err != nil {
					return err
				}
				if len(ss) > 0 {
					sha1s = appendUniqueSha1(sha1s, ss)
				}
			}
			if len(sha1s) > 0 {
				kvb.sha1Batch.Set(rom.Sha1, sha1s)
			}
		}
		return nil
	} else {
		glog.V(4).Infof("rom %s not referenced by any dats, building artificial dat", rom.Name)
	}

	dat := new(types.Dat)
	dat.Artificial = true
	dat.Generation = kvb.db.generation
	dat.Name = fmt.Sprintf("Artificial Dat for %s", rom.Name)
	dat.Path = rom.Path
	game := new(types.Game)
	game.Roms = []*types.Rom{rom}
	dat.Games = []*types.Game{game}

	var buf bytes.Buffer

	gobEncoder := gob.NewEncoder(&buf)
	err = gobEncoder.Encode(dat)
	if err != nil {
		return err
	}

	hh := sha1.New()
	_, err = io.Copy(hh, &buf)
	if err != nil {
		return err
	}

	return kvb.IndexDat(dat, hh.Sum(nil))
}

func (kvb *kvBatch) IndexDat(dat *types.Dat, sha1Bytes []byte) error {
	glog.Infof("indexing dat %s", dat.Name)

	if sha1Bytes == nil {
		return fmt.Errorf("sha1 is nil for %s", dat.Path)
	}

	dat.Generation = kvb.db.generation

	var buf bytes.Buffer

	gobEncoder := gob.NewEncoder(&buf)
	err := gobEncoder.Encode(dat)
	if err != nil {
		return err
	}

	var exists bool

	if dat.Artificial {
		exists = false
	} else {
		existsSha1, err := kvb.db.datsDB.Exists(sha1Bytes)
		if err != nil {
			return fmt.Errorf("failed to lookup sha1 indexing dats: %v", err)
		}
		exists = existsSha1
	}

	kvb.datsBatch.Set(sha1Bytes, buf.Bytes())

	kvb.size += int64(sha1.Size + buf.Len())

	if !exists {
		for _, g := range dat.Games {
			glog.Infof("indexing game %s", g.Name)
			for _, r := range g.Roms {
				if r.Sha1 != nil {
					err = kvb.sha1Batch.Append(r.Sha1, sha1Bytes)
					if err != nil {
						return err
					}
					kvb.size += int64(sha1.Size)
				}

				if r.Md5 != nil {
					err = kvb.md5Batch.Append(r.Md5, sha1Bytes)
					if err != nil {
						return err
					}
					kvb.size += int64(sha1.Size)

					if r.Sha1 != nil {
						if glog.V(4) {
							glog.Infof("declaring md5 %s -> sha1 %s mapping", hex.EncodeToString(r.Md5), hex.EncodeToString(r.Sha1))
						}
						err = kvb.md5sha1Batch.Append(r.Md5, r.Sha1)
						if err != nil {
							return err
						}
						kvb.size += int64(sha1.Size)
					}
				}

				if r.Crc != nil {
					err = kvb.crcBatch.Append(r.Crc, sha1Bytes)
					if err != nil {
						return err
					}
					kvb.size += int64(sha1.Size)

					if r.Sha1 != nil {
						if glog.V(4) {
							glog.Infof("declaring crc %s -> sha1 %s mapping", hex.EncodeToString(r.Crc), hex.EncodeToString(r.Sha1))
						}
						err = kvb.crcsha1Batch.Append(r.Crc, r.Sha1)
						if err != nil {
							return err
						}
						kvb.size += int64(sha1.Size)
					}
				}
			}
		}
	}
	return nil
}

func (kvb *kvBatch) Size() int64 {
	return kvb.size
}

func dbSha1Append(db KVStore, batch KVBatch, key, sha1Bytes []byte) error {
	if key == nil {
		return nil
	}

	vBytes, err := db.Get(key)
	if err != nil {
		return fmt.Errorf("failed to lookup in dbSha1Append: %v", err)
	}

	found := false
	for i := 0; i < len(vBytes); i += sha1.Size {
		if bytes.Equal(sha1Bytes, vBytes[i:i+sha1.Size]) {
			found = true
			break
		}
	}

	if !found {
		vBytes = append(vBytes, sha1Bytes...)
		batch.Set(key, vBytes)
	}
	return nil
}

func printSha1s(vBytes []byte) string {
	var buf bytes.Buffer

	buf.WriteString("[")
	first := true
	for i := 0; i < len(vBytes); i += sha1.Size {
		sha1 := hex.EncodeToString(vBytes[i : i+sha1.Size])
		if first {
			first = false
		} else {
			buf.WriteString(", ")
		}
		buf.WriteString(sha1)
	}
	buf.WriteString("]")
	return buf.String()
}

func (kvdb *kvStore) DebugGet(key []byte) string {
	var buf bytes.Buffer

	switch len(key) {
	case md5.Size:
		sha1s, err := kvdb.md5DB.Get(key)
		if err != nil {
			glog.Errorf("error getting from md5DB: %v", err)
		} else {
			buf.WriteString(fmt.Sprintf("md5DB -> %s\n", printSha1s(sha1s)))
		}

		sha1s, err = kvdb.md5sha1DB.Get(key)
		if err != nil {
			glog.Errorf("error getting from md5sha1DB: %v", err)
		} else {
			buf.WriteString(fmt.Sprintf("md5sha1DB -> %s\n", printSha1s(sha1s)))
		}
	case crc32.Size:
		sha1s, err := kvdb.crcDB.Get(key)
		if err != nil {
			glog.Errorf("error getting from crcDB: %v", err)
		} else {
			buf.WriteString(fmt.Sprintf("crcDB -> %s\n", printSha1s(sha1s)))
		}

		sha1s, err = kvdb.crcsha1DB.Get(key)
		if err != nil {
			glog.Errorf("error getting from crcsha1DB: %v", err)
		} else {
			buf.WriteString(fmt.Sprintf("crcsha1DB -> %s\n", printSha1s(sha1s)))
		}
	case sha1.Size:
		sha1s, err := kvdb.sha1DB.Get(key)
		if err != nil {
			glog.Errorf("error getting from sha1DB: %v", err)
		} else {
			buf.WriteString(fmt.Sprintf("sha1DB -> %s\n", printSha1s(sha1s)))
		}
	default:
		glog.Errorf("found unknown hash size: %d", len(key))
		return ""
	}

	return buf.String()
}
