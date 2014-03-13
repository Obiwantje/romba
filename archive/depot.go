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

package archive

import (
	"bufio"
	"bytes"
	"container/ring"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/golang/glog"

	"github.com/dustin/go-humanize"

	"github.com/uwedeportivo/torrentzip"
	"github.com/uwedeportivo/torrentzip/cgzip"
	"github.com/uwedeportivo/torrentzip/czip"

	"github.com/uwedeportivo/romba/db"
	"github.com/uwedeportivo/romba/types"
	"github.com/uwedeportivo/romba/worker"
)

type Depot struct {
	roots    []string
	sizes    []int64
	maxSizes []int64
	romDB    db.RomDB
	lock     *sync.Mutex
	start    int
}

type completed struct {
	path        string
	workerIndex int
}

type archiveWorker struct {
	depot        *Depot
	hh           *Hashes
	md5crcBuffer []byte
	index        int
	pm           *archiveMaster
}

type archiveMaster struct {
	depot           *Depot
	resumePath      string
	numWorkers      int
	pt              worker.ProgressTracker
	soFar           chan *completed
	resumeLogFile   *os.File
	resumeLogWriter *bufio.Writer
	includezips     bool
	onlyneeded      bool
}

func NewDepot(roots []string, maxSize []int64, romDB db.RomDB) (*Depot, error) {
	glog.Info("Depot init")
	depot := new(Depot)
	depot.roots = make([]string, len(roots))
	depot.sizes = make([]int64, len(roots))
	depot.maxSizes = make([]int64, len(roots))

	copy(depot.roots, roots)
	copy(depot.maxSizes, maxSize)

	for k, root := range depot.roots {
		glog.Infof("establishing size of %s", root)
		size, err := establishSize(root)
		if err != nil {
			return nil, err
		}
		depot.sizes[k] = size
	}

	glog.Info("Initializing Depot with the following roots")

	for k, root := range depot.roots {
		glog.Infof("root = %s, maxSize = %s, size = %s", root,
			humanize.Bytes(uint64(depot.maxSizes[k])), humanize.Bytes(uint64(depot.sizes[k])))
	}

	depot.romDB = romDB
	depot.lock = new(sync.Mutex)
	glog.Info("Depot init finished")
	return depot, nil
}

func extractResumePoint(resumePath string, numWorkers int) (string, error) {
	// we need the last n lines from the file, where n == numWorkers
	f, err := os.Open(resumePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", err
	}

	bufSize := int64(10240)
	if bufSize > fi.Size() {
		bufSize = fi.Size()
	}

	buf := make([]byte, bufSize)
	_, err = f.ReadAt(buf, fi.Size()-bufSize)
	if err != nil {
		return "", err
	}

	rng := ring.New(numWorkers)
	reader := bufio.NewReader(bytes.NewReader(buf))

	numLines := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		if len(line) > 0 {
			numLines++
			rng.Value = line
			rng = rng.Next()
		}
		if err == io.EOF {
			break
		}
	}

	if numLines == 0 {
		return "", fmt.Errorf("could not extract a resume point from %s, file seems empty", resumePath)
	}

	if numLines < numWorkers {
		glog.Warningf("extracting resume point from %s: expected %d lines, got %d", resumePath, numWorkers, numLines)
	}

	lines := make([]string, numLines)
	lineCursor := 0

	rng.Do(func(v interface{}) {
		line := v.(string)
		if len(line) > 0 {
			lines[lineCursor] = line
			lineCursor++
		}
	})

	sort.Strings(lines)
	return lines[0], nil
}

func (depot *Depot) Archive(paths []string, resumePath string, includezips bool, onlyneeded bool, numWorkers int,
	logDir string, pt worker.ProgressTracker) (string, error) {

	resumeLogPath := filepath.Join(logDir, fmt.Sprintf("archive-resume-%s.log", time.Now().Format("2006-01-02-15_04_05")))
	resumeLogFile, err := os.Create(resumeLogPath)
	if err != nil {
		return "", err
	}
	resumeLogWriter := bufio.NewWriter(resumeLogFile)

	resumePoint := ""
	if len(resumePath) > 0 {
		resumePoint, err = extractResumePoint(resumePath, numWorkers)
		if err != nil {
			return "", err
		}
	}

	pm := new(archiveMaster)
	pm.depot = depot
	pm.resumePath = resumePoint
	pm.pt = pt
	pm.numWorkers = numWorkers
	pm.soFar = make(chan *completed)
	pm.resumeLogWriter = resumeLogWriter
	pm.resumeLogFile = resumeLogFile
	pm.includezips = includezips
	pm.onlyneeded = onlyneeded

	go pm.loopObserver()

	return worker.Work("archive roms", paths, pm)
}

func (depot *Depot) SHA1InDepot(sha1Hex string) (bool, error) {
	for _, root := range depot.roots {
		rompath := pathFromSha1HexEncoding(root, sha1Hex, gzipSuffix)
		exists, err := PathExists(rompath)
		if err != nil {
			return false, err
		}

		if exists {
			return true, nil
		}
	}
	return false, nil
}

func (depot *Depot) OpenRomGZ(rom *types.Rom) (io.ReadCloser, error) {
	if rom.Sha1 == nil {
		return nil, fmt.Errorf("cannot open rom %s because SHA1 is missing", rom.Name)
	}

	if len(rom.Sha1) == sha1.Size {
		sha1Hex := hex.EncodeToString(rom.Sha1)

		for _, root := range depot.roots {
			rompath := pathFromSha1HexEncoding(root, sha1Hex, gzipSuffix)
			exists, err := PathExists(rompath)
			if err != nil {
				return nil, err
			}

			if exists {
				return os.Open(rompath)
			}
		}
	} else {
		if glog.V(2) {
			glog.Infof("searching for the right file for rom %s because of hash collisions", rom.Name)
		}
		for i := 0; i < len(rom.Sha1); i += sha1.Size {
			sha1Hex := hex.EncodeToString(rom.Sha1[i : i+sha1.Size])

			if glog.V(3) {
				glog.Infof("trying SHA1 %s", sha1Hex)
			}

			for _, root := range depot.roots {
				rompath := pathFromSha1HexEncoding(root, sha1Hex, gzipSuffix)
				exists, err := PathExists(rompath)
				if err != nil {
					return nil, err
				}

				if exists {
					// double check that it matches crc or md5
					if rom.Crc != nil || rom.Md5 != nil {
						hh, err := HashesForGZFile(rompath)
						if err != nil {
							return nil, err
						}

						if rom.Md5 != nil && bytes.Equal(rom.Md5, hh.Md5) {
							return os.Open(rompath)
						}

						if rom.Crc != nil && bytes.Equal(rom.Crc, hh.Crc) {
							return os.Open(rompath)
						}

					} else {
						if glog.V(2) {
							glog.Warningf("rom %s with collision SHA1 and no other hash to disambigue", rom.Name)
						}
						return os.Open(rompath)
					}
				}
			}
		}
	}

	return nil, nil
}

func (depot *Depot) BuildDat(dat *types.Dat, outpath string) (bool, error) {
	datPath := filepath.Join(outpath, dat.Name)

	err := os.Mkdir(datPath, 0777)
	if err != nil {
		return false, err
	}

	var fixDat *types.Dat

	for _, game := range dat.Games {
		gamePath := filepath.Join(datPath, game.Name+zipSuffix)
		fixGame, foundRom, err := depot.buildGame(game, gamePath)
		if err != nil {
			return false, err
		}
		if fixGame != nil {
			if fixDat == nil {
				fixDat = new(types.Dat)
				fixDat.Name = dat.Name
				fixDat.Description = dat.Description
				fixDat.Path = dat.Path
			}
			fixDat.Games = append(fixDat.Games, fixGame)
		}
		if !foundRom {
			err = os.Remove(gamePath)
			if err != nil {
				return false, err
			}
		}
	}

	if fixDat != nil {
		fixDatPath := filepath.Join(outpath, fixPrefix+dat.Name+datSuffix)

		fixFile, err := os.Create(fixDatPath)
		if err != nil {
			return false, err
		}
		defer fixFile.Close()

		fixWriter := bufio.NewWriter(fixFile)
		defer fixWriter.Flush()

		err = types.ComposeCompliantDat(fixDat, fixWriter)
		if err != nil {
			return false, err
		}
	}

	return fixDat == nil, nil
}

func (depot *Depot) buildGame(game *types.Game, gamePath string) (*types.Game, bool, error) {
	gameFile, err := os.Create(gamePath)
	if err != nil {
		return nil, false, err
	}
	defer gameFile.Close()

	gameTorrent, err := torrentzip.NewWriter(gameFile)
	if err != nil {
		return nil, false, err
	}
	defer gameTorrent.Close()

	var fixGame *types.Game

	foundRom := false

	for _, rom := range game.Roms {
		if rom.Sha1 == nil {
			if glog.V(2) {
				glog.Warningf("game %s has rom with missing SHA1 %s", game.Name, rom.Name)
			}
			if fixGame == nil {
				fixGame = new(types.Game)
				fixGame.Name = game.Name
				fixGame.Description = game.Description
			}

			fixGame.Roms = append(fixGame.Roms, rom)
			continue
		}

		romGZ, err := depot.OpenRomGZ(rom)
		if err != nil {
			return nil, false, err
		}

		if romGZ == nil {
			if glog.V(2) {
				glog.Warningf("game %s has missing rom %s (sha1 %s)", game.Name, rom.Name, hex.EncodeToString(rom.Sha1))
			}
			if fixGame == nil {
				fixGame = new(types.Game)
				fixGame.Name = game.Name
				fixGame.Description = game.Description
			}

			fixGame.Roms = append(fixGame.Roms, rom)
			continue
		}

		foundRom = true
		src, err := cgzip.NewReader(romGZ)
		if err != nil {
			return nil, false, err
		}

		dst, err := gameTorrent.Create(rom.Name)
		if err != nil {
			return nil, false, err
		}

		_, err = io.Copy(dst, src)
		if err != nil {
			return nil, false, err
		}

		src.Close()
		romGZ.Close()
	}
	return fixGame, foundRom, nil
}

func (pm *archiveMaster) Accept(path string) bool {
	if pm.resumePath != "" {
		return path > pm.resumePath
	}
	return true
}

func (pm *archiveMaster) NewWorker(workerIndex int) worker.Worker {
	return &archiveWorker{
		depot:        pm.depot,
		hh:           newHashes(),
		md5crcBuffer: make([]byte, md5.Size+crc32.Size),
		index:        workerIndex,
		pm:           pm,
	}
}

func (pm *archiveMaster) NumWorkers() int {
	return pm.numWorkers
}

func (pm *archiveMaster) ProgressTracker() worker.ProgressTracker {
	return pm.pt
}

func (pm *archiveMaster) FinishUp() error {
	pm.soFar <- &completed{
		workerIndex: -1,
	}

	pm.depot.writeSizes()
	pm.resumeLogWriter.Flush()

	return pm.resumeLogFile.Close()
}

func (pm *archiveMaster) Start() error {
	return nil
}

func (pm *archiveMaster) Scanned(numFiles int, numBytes int64, commonRootPath string) {}

func (depot *Depot) reserveRoot(size int64) (int, error) {
	depot.lock.Lock()
	defer depot.lock.Unlock()

	for i := depot.start; i < len(depot.roots); i++ {
		if depot.sizes[i]+size < depot.maxSizes[i] {
			depot.sizes[i] += size
			return i, nil
		} else if depot.sizes[i] >= depot.maxSizes[i] {
			depot.start = i
		}
	}

	glog.Error("Depot with the following roots ran out of disk space")
	for k, root := range depot.roots {
		glog.Errorf("root = %s, maxSize = %s, size = %s", root,
			humanize.Bytes(uint64(depot.maxSizes[k])), humanize.Bytes(uint64(depot.sizes[k])))
	}

	return -1, fmt.Errorf("depot ran out of disk space")
}

func (depot *Depot) writeSizes() {
	depot.lock.Lock()
	defer depot.lock.Unlock()

	for k, root := range depot.roots {
		err := writeSizeFile(root, depot.sizes[k])
		if err != nil {
			glog.Errorf("failed to write size file into %s: %v\n", root, err)
		}
	}
}

func (depot *Depot) adjustSize(index int, delta int64) {
	depot.lock.Lock()
	defer depot.lock.Unlock()

	depot.sizes[index] += delta
}

func (w *archiveWorker) Process(path string, size int64) error {
	var err error

	pathext := filepath.Ext(path)

	if pathext == zipSuffix {
		_, err = w.archiveZip(path, size, w.pm.includezips)
	} else if pathext == gzipSuffix {
		_, err = w.archiveGzip(path, size, w.pm.includezips)
	} else {
		_, err = w.archiveRom(path, size)
	}

	if err != nil {
		return err
	}

	w.pm.soFar <- &completed{
		path:        path,
		workerIndex: w.index,
	}
	return nil
}

func (w *archiveWorker) Close() error {
	return nil
}

type readerOpener func() (io.ReadCloser, error)

func (w *archiveWorker) archive(ro readerOpener, name, path string, size int64) (int64, error) {
	r, err := ro()
	if err != nil {
		return 0, err
	}

	br := bufio.NewReader(r)

	err = w.hh.forReader(br)
	if err != nil {
		r.Close()
		return 0, err
	}
	err = r.Close()
	if err != nil {
		return 0, err
	}

	copy(w.md5crcBuffer[0:md5.Size], w.hh.Md5)
	copy(w.md5crcBuffer[md5.Size:], w.hh.Crc)

	rom := new(types.Rom)
	rom.Crc = make([]byte, crc32.Size)
	rom.Md5 = make([]byte, md5.Size)
	rom.Sha1 = make([]byte, sha1.Size)
	copy(rom.Crc, w.hh.Crc)
	copy(rom.Md5, w.hh.Md5)
	copy(rom.Sha1, w.hh.Sha1)
	rom.Name = name
	rom.Size = size
	rom.Path = path

	if w.pm.onlyneeded {
		dats, err := w.depot.romDB.DatsForRom(rom)
		if err != nil {
			return 0, err
		}

		needed := false

		for _, dat := range dats {
			if !dat.Artificial {
				needed = true
				break
			}
		}
		if !needed {
			return 0, nil
		}
	}

	err = w.depot.romDB.IndexRom(rom)
	if err != nil {
		return 0, err
	}

	sha1Hex := hex.EncodeToString(w.hh.Sha1)
	exists, err := w.depot.SHA1InDepot(sha1Hex)
	if err != nil {
		return 0, err
	}

	if exists {
		return 0, nil
	}

	estimatedCompressedSize := size / 5

	root, err := w.depot.reserveRoot(estimatedCompressedSize)
	if err != nil {
		return 0, err
	}

	outpath := pathFromSha1HexEncoding(w.depot.roots[root], sha1Hex, gzipSuffix)

	r, err = ro()
	if err != nil {
		return 0, err
	}
	defer r.Close()

	compressedSize, err := archive(outpath, r, w.md5crcBuffer)
	if err != nil {
		return 0, err
	}

	w.depot.adjustSize(root, compressedSize-estimatedCompressedSize)
	return compressedSize, nil
}

func (w *archiveWorker) archiveZip(inpath string, size int64, addZipItself bool) (int64, error) {
	if glog.V(2) {
		glog.Infof("archiving zip %s ", inpath)
	}
	zr, err := czip.OpenReader(inpath)
	if err != nil {
		return 0, err
	}
	defer zr.Close()

	var compressedSize int64

	for _, zf := range zr.File {
		if glog.V(2) {
			glog.Infof("archiving zip %s: file %s ", inpath, zf.Name)
		}
		cs, err := w.archive(func() (io.ReadCloser, error) { return zf.Open() },
			zf.FileInfo().Name(), filepath.Join(inpath, zf.FileInfo().Name()), zf.FileInfo().Size())
		if err != nil {
			glog.Errorf("zip error %s: %v", inpath, err)
			return 0, err
		}
		compressedSize += cs
	}

	if addZipItself {
		cs, err := w.archive(func() (io.ReadCloser, error) { return os.Open(inpath) }, filepath.Base(inpath), inpath, size)
		if err != nil {
			return 0, err
		}
		compressedSize += cs
	}
	return compressedSize, nil
}

func stripExt(path string) string {
	ext := filepath.Ext(path)
	return path[:len(path)-len(ext)]
}

type gzipReadCloser struct {
	file *os.File
	zr   *cgzip.Reader
}

func (grc *gzipReadCloser) Close() error {
	err := grc.zr.Close()
	if err != nil {
		grc.file.Close()
		return err
	}
	return grc.file.Close()
}

func (grc *gzipReadCloser) Read(p []byte) (n int, err error) {
	return grc.zr.Read(p)
}

func openGzipReadCloser(inpath string) (io.ReadCloser, error) {
	f, err := os.Open(inpath)
	if err != nil {
		return nil, err
	}
	_, err = f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	zr, err := cgzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}

	return &gzipReadCloser{
		file: f,
		zr:   zr,
	}, nil
}

func (w *archiveWorker) archiveGzip(inpath string, size int64, addZipItself bool) (int64, error) {
	if addZipItself {
		return w.archiveRom(inpath, size)
	}

	return w.archive(func() (io.ReadCloser, error) { return openGzipReadCloser(inpath) },
		filepath.Base(inpath), stripExt(inpath), size)
}

func (w *archiveWorker) archiveRom(inpath string, size int64) (int64, error) {
	return w.archive(func() (io.ReadCloser, error) { return os.Open(inpath) }, filepath.Base(inpath), inpath, size)
}

func (pm *archiveMaster) writeResumeLogEntry(comps []string) {
	if comps[0] != "" {
		sort.Strings(comps)
		fmt.Fprint(pm.resumeLogWriter, "%s\n", comps[0])
		pm.depot.writeSizes()
	}
}

func (pm *archiveMaster) loopObserver() {
	ticker := time.NewTicker(time.Minute * 1)
	comps := make([]string, pm.numWorkers)

	for {
		select {
		case comp := <-pm.soFar:
			if comp.workerIndex == -1 {
				pm.writeResumeLogEntry(comps)
				break
			}
			comps[comp.workerIndex] = comp.path
		case <-ticker.C:
			pm.writeResumeLogEntry(comps)
		}
	}

	ticker.Stop()
}
