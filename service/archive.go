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

package service

import (
	"errors"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/golang/glog"
	"github.com/uwedeportivo/commander"
)

func findLatestResumeLog(logDir string) (string, error) {
	lfs, err := ioutil.ReadDir(logDir)
	if err != nil {
		return "", err
	}

	latestTs, _ := time.Parse("2006-01-02-15_04_05", "2010-01-02-15_04_05")
	latestFile := ""

	for _, lf := range lfs {
		//archive-resume-2014-05-17-15_48_50.log
		name := lf.Name()
		if strings.HasPrefix(name, "archive-resume-") && strings.HasSuffix(name, ".log") {
			dateStr := name[15 : len(name)-4]
			tstamp, err := time.Parse("2006-01-02-15_04_05", dateStr)
			if err != nil {
				return "", err
			}
			if tstamp.After(latestTs) {
				latestTs = tstamp
				latestFile = filepath.Join(logDir, name)
			}
		}
	}

	return latestFile, nil
}

func (rs *RombaService) startArchive(cmd *commander.Command, args []string) error {
	rs.jobMutex.Lock()
	defer rs.jobMutex.Unlock()

	if len(args) == 0 {
		return nil
	}

	if rs.busy {
		p := rs.pt.GetProgress()

		fmt.Fprintf(cmd.Stdout, "still busy with %s: (%d of %d files) and (%s of %s) \n", rs.jobName,
			p.FilesSoFar, p.TotalFiles, humanize.Bytes(uint64(p.BytesSoFar)), humanize.Bytes(uint64(p.TotalBytes)))
		return nil
	}

	rs.pt.Reset()
	rs.busy = true
	rs.jobName = "archive"

	resume := cmd.Flag.Lookup("resume").Value.Get().(string)
	if resume == "latest" {
		latestResume, err := findLatestResumeLog(rs.logDir)
		if err != nil {
			glog.Errorf("error finding the latest resume point: %v", err)
			return err
		}
		resume = latestResume
		if len(resume) == 0 {
			glog.Errorf("no resume file found")
			return errors.New("no resume file found")
		}
	}

	go func() {
		glog.Infof("service starting archive")
		rs.broadCastProgress(time.Now(), true, false, "")
		ticker := time.NewTicker(time.Second * 5)
		stopTicker := make(chan bool)
		go func() {
			glog.Infof("starting progress broadcaster")
			for {
				select {
				case t := <-ticker.C:
					rs.broadCastProgress(t, false, false, "")
				case <-stopTicker:
					glog.Info("stopped progress broadcaster")
					return
				}
			}
		}()

		includezips := cmd.Flag.Lookup("include-zips").Value.Get().(bool)
		includegzips := cmd.Flag.Lookup("include-gzips").Value.Get().(bool)
		include7zips := cmd.Flag.Lookup("include-7zips").Value.Get().(bool)
		onlyneeded := cmd.Flag.Lookup("only-needed").Value.Get().(bool)
		numWorkers := cmd.Flag.Lookup("workers").Value.Get().(int)

		endMsg, err := rs.depot.Archive(args, resume, includezips, includegzips, include7zips,
			onlyneeded, numWorkers, rs.logDir, rs.pt)
		if err != nil {
			glog.Errorf("error archiving: %v", err)
		}

		ticker.Stop()
		stopTicker <- true

		rs.jobMutex.Lock()
		rs.busy = false
		rs.jobName = ""
		rs.jobMutex.Unlock()

		rs.broadCastProgress(time.Now(), false, true, endMsg)
		glog.Infof("service finished archiving")
	}()

	fmt.Fprintf(cmd.Stdout, "started archiving")
	return nil
}
