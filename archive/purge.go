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
	"encoding/hex"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/golang/glog"

	"github.com/uwedeportivo/romba/types"
	"github.com/uwedeportivo/romba/worker"
)

type purgeWorker struct {
	depot *Depot
	index int
	pm    *purgeMaster
}

type purgeMaster struct {
	depot      *Depot
	numWorkers int
	pt         worker.ProgressTracker
	backupDir  string
}

func (depot *Depot) Purge(backupDir string, numWorkers int, pt worker.ProgressTracker) (string, error) {
	pm := new(purgeMaster)
	pm.depot = depot
	pm.pt = pt
	pm.numWorkers = numWorkers

	absBackupDir, err := filepath.Abs(backupDir)
	if err != nil {
		return "", err
	}

	pm.backupDir = absBackupDir

	if backupDir == "" {
		return "", errors.New("no backup dir specified")
	}

	err = os.MkdirAll(backupDir, 0777)
	if err != nil {
		return "", err
	}

	return worker.Work("purge roms", depot.roots, pm)
}

func (pm *purgeMaster) Accept(path string) bool {
	return filepath.Ext(path) == gzipSuffix
}

func (pm *purgeMaster) CalculateWork() bool {
	return false
}

func (pm *purgeMaster) NewWorker(workerIndex int) worker.Worker {
	return &purgeWorker{
		depot: pm.depot,
		index: workerIndex,
		pm:    pm,
	}
}

func (pm *purgeMaster) NumWorkers() int {
	return pm.numWorkers
}

func (pm *purgeMaster) ProgressTracker() worker.ProgressTracker {
	return pm.pt
}

func (pm *purgeMaster) FinishUp() error {
	pm.depot.writeSizes()
	return nil
}

func (pm *purgeMaster) Start() error {
	return nil
}

func (pm *purgeMaster) Scanned(numFiles int, numBytes int64, commonRootPath string) {}

func (w *purgeWorker) Process(inpath string, size int64) error {
	rom, err := RomFromGZDepotFile(inpath)
	if err != nil {
		return err
	}

	_, hh, err := w.pm.depot.SHA1InDepot(hex.EncodeToString(rom.Sha1))
	if err != nil {
		return err
	}

	rom.Md5 = hh.Md5
	rom.Crc = hh.Crc

	dats, err := w.pm.depot.romDB.DatsForRom(rom)
	if err != nil {
		return err
	}

	used := false
	var realDat *types.Dat

	for _, dat := range dats {
		if !dat.Artificial && dat.Generation == w.pm.depot.romDB.Generation() {
			used = true
			break
		}
		if !dat.Artificial {
			realDat = dat
		}
	}

	if !used {
		destPath := path.Join(w.pm.backupDir, "uncategorized", filepath.Base(inpath))

		if realDat != nil && realDat.Path != "" {
			commonRoot := worker.CommonRoot(w.pm.backupDir, realDat.Path)
			destPath = path.Join(w.pm.backupDir,
				strings.TrimSuffix(strings.TrimPrefix(realDat.Path, commonRoot), filepath.Ext(realDat.Path)),
				filepath.Base(inpath))
		}
		glog.V(2).Infof("purging %s, moving to %s", inpath, destPath)
		err = worker.Mv(inpath, destPath)
		if err != nil {
			return err
		}
		index := -1
		for i, depotRoot := range w.pm.depot.roots {
			if strings.HasPrefix(inpath, depotRoot) {
				index = i
				break
			}
		}

		if index != -1 {
			w.pm.depot.adjustSize(index, -size)
		}
	}
	return nil
}

func (w *purgeWorker) Close() error {
	return nil
}
