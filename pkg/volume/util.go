/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package volume

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	// ExportID = 152 is being reserved
	// so that 152.152 is not used as filesystem_id in nfs-ganesha export configuration
	// 152.152 is the default pseudo root filesystem ID
	// Ref: https://github.com/kubernetes-sigs/nfs-ganesha-server-and-external-provisioner/issues/7
	ReservedExportID = 152
)

// generateID generates a unique exportID to assign an export
func generateID(mutex *sync.Mutex, ids map[uint16]bool) uint16 {
	mutex.Lock()
	id := uint16(1)
	for ; id <= math.MaxUint16; id++ {
		if _, ok := ids[id]; !ok {
			if id == ReservedExportID {
				continue
			}
			break
		}
	}
	ids[id] = true
	mutex.Unlock()
	return id
}

func deleteID(mutex *sync.Mutex, ids map[uint16]bool, id uint16) {
	mutex.Lock()
	delete(ids, id)
	mutex.Unlock()
}

// getExistingIDs populates a map with existing ids found in the given config
// file using the given regexp. Regexp must have a "digits" submatch.
func getExistingIDs(config string, re *regexp.Regexp) (map[uint16]bool, error) {
	ids := map[uint16]bool{}

	digitsRe := "([0-9]+)"
	if !strings.Contains(re.String(), digitsRe) {
		return ids, fmt.Errorf("regexp %s doesn't contain digits submatch %s", re.String(), digitsRe)
	}

	read, err := ioutil.ReadFile(config)
	if err != nil {
		return ids, err
	}

	allMatches := re.FindAllSubmatch(read, -1)
	for _, match := range allMatches {
		digits := match[1]
		if id, err := strconv.ParseUint(string(digits), 10, 16); err == nil {
			ids[uint16(id)] = true
		}
	}

	return ids, nil
}

func getExports(config string) (map[uint16]string, error) {
	idPathMap := map[uint16]string{}

	f, err := os.Open(config)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "Export_Id") {
			tp := strings.Split(line, " = ")
			if len(tp) != 2 {
				continue
			}
			digits := strings.TrimSuffix(strings.TrimSpace(tp[1]), ";")
			if id, err := strconv.ParseUint(digits, 10, 16); err == nil {
				if scanner.Scan() {
					line := scanner.Text()
					if strings.Contains(line, "Path") {
						tp := strings.Split(line, " = ")
						if len(tp) != 2 {
							continue
						}
						path := strings.TrimSuffix(strings.TrimSpace(tp[1]), ";")
						idPathMap[uint16(id)] = path
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return idPathMap, nil
}

func addToFile(mutex *sync.Mutex, path string, toAdd string) error {
	mutex.Lock()

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		mutex.Unlock()
		return err
	}
	defer file.Close()

	if _, err = file.WriteString(toAdd); err != nil {
		mutex.Unlock()
		return err
	}
	file.Sync()

	mutex.Unlock()
	return nil
}

func removeFromFile(mutex *sync.Mutex, path string, toRemove string) error {
	mutex.Lock()

	read, err := ioutil.ReadFile(path)
	if err != nil {
		mutex.Unlock()
		return err
	}

	removed := strings.Replace(string(read), toRemove, "", -1)
	err = ioutil.WriteFile(path, []byte(removed), 0)
	if err != nil {
		mutex.Unlock()
		return err
	}

	mutex.Unlock()
	return nil
}
