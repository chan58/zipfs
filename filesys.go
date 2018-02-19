// Copyright 2013-2018 C Hansen
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package zipfs

import (
	"archive/zip"
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// ZipFS implements the http.FileSystem interface.
type ZipFS struct {
	readerAt  io.ReaderAt
	closer    io.Closer
	reader    *zip.Reader
	fileInfos fileInfoMap
}

var (
	errFileClosed       = errors.New("file closed")
	errFileSystemClosed = errors.New("filesystem closed")
	errNotDirectory     = errors.New("not a directory")
	errDirectory        = errors.New("is a directory")
)

// New instantiates and returns a Zip filesystem
func New(name string) (*ZipFS, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}
	zipReader, err := zip.NewReader(file, fi.Size())
	if err != nil {
		return nil, err
	}

	// Separate the file into an io.ReaderAt and an io.Closer.
	// Earlier versions of the code allowed for opening a filesystem
	// just with an io.ReaderAt.
	//
	// The zip.Reader is not actually used outside of this function so it probably
	// does not need to be in the ZipFS structure. Keeping it there for now
	// but may remove it in future.
	fs := &ZipFS{
		closer:    file,
		readerAt:  file,
		reader:    zipReader,
		fileInfos: fileInfoMap{},
	}
	for _, zf := range fs.reader.File {
		fi := fs.fileInfos.FindOrCreate(zf.Name)
		fi.zipFile = zf
		fiParent := fs.fileInfos.FindOrCreateParent(zf.Name)
		fiParent.fileInfos = append(fiParent.fileInfos, fi)
	}
	// Sort fileInfos in each directory.
	for _, fi := range fs.fileInfos {
		if len(fi.fileInfos) > 1 {
			sort.Sort(fi.fileInfos)
		}
	}
	return fs, nil
}

// Open a path within the Zip filesystem for read
func (fs *ZipFS) Open(name string) (http.File, error) {
	fi, err := fs.openFileInfo(name)
	if err != nil {
		return nil, err
	}
	return fi.openReader(name), nil
}

// Close an open file path
func (fs *ZipFS) Close() error {
	fs.reader = nil
	fs.readerAt = nil
	var err error
	if fs.closer != nil {
		err = fs.closer.Close()
		fs.closer = nil
	}
	fs.fileInfos = nil
	return err
}

type fileInfoList []*fileInfo

func (fl fileInfoList) Len() int {
	return len(fl)
}

func (fl fileInfoList) Less(i, j int) bool {
	name1 := fl[i].Name()
	name2 := fl[j].Name()
	return name1 < name2
}

func (fl fileInfoList) Swap(i, j int) {
	fi := fl[i]
	fl[i] = fl[j]
	fl[j] = fi
}

func (fs *ZipFS) openFileInfo(name string) (*fileInfo, error) {
	if fs.readerAt == nil {
		return nil, errFileSystemClosed
	}
	name = path.Clean(name)
	trimmedName := strings.TrimLeft(name, "/")
	fi := fs.fileInfos[trimmedName]
	if fi == nil {
		return nil, &os.PathError{Op: "Open", Path: name, Err: os.ErrNotExist}
	}
	return fi, nil
}

// fileInfoMap keeps track of fileInfos
type fileInfoMap map[string]*fileInfo

func (fm fileInfoMap) FindOrCreate(name string) *fileInfo {
	strippedName := strings.TrimRight(name, "/")
	fi := fm[name]
	if fi == nil {
		fi = &fileInfo{
			name: name,
		}
		fm[name] = fi
		if strippedName != name {
			// directories get two entries: with and without trailing slash
			fm[strippedName] = fi
		}
	}
	return fi
}

func (fm fileInfoMap) FindOrCreateParent(name string) *fileInfo {
	strippedName := strings.TrimRight(name, "/")
	dirName := path.Dir(strippedName)
	if dirName == "." {
		dirName = "/"
	} else if !strings.HasSuffix(dirName, "/") {
		dirName = dirName + "/"
	}
	return fm.FindOrCreate(dirName)
}

// fileInfo implements the os.FileInfo interface.
type fileInfo struct {
	name      string
	fs        *ZipFS
	zipFile   *zip.File
	fileInfos fileInfoList
	tempPath  string
	mutex     sync.Mutex
}

func (fi *fileInfo) Name() string {
	return path.Base(fi.name)
}

func (fi *fileInfo) Size() int64 {
	if fi.zipFile == nil {
		return 0
	}
	if fi.zipFile.UncompressedSize64 == 0 {
		return int64(fi.zipFile.UncompressedSize)
	}
	return int64(fi.zipFile.UncompressedSize64)
}

func (fi *fileInfo) Mode() os.FileMode {
	if fi.zipFile == nil || fi.IsDir() {
		return 0555 | os.ModeDir
	}
	return 0444
}

var dirTime = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func (fi *fileInfo) ModTime() time.Time {
	if fi.zipFile == nil {
		return dirTime
	}
	return fi.zipFile.ModTime()
}

func (fi *fileInfo) IsDir() bool {
	if fi.zipFile == nil {
		return true
	}
	return fi.zipFile.Mode().IsDir()
}

func (fi *fileInfo) Sys() interface{} {
	return fi.zipFile
}

func (fi *fileInfo) openReader(name string) *fileReader {
	return &fileReader{
		fileInfo: fi,
		name:     name,
	}
}

func (fi *fileInfo) readdir() ([]os.FileInfo, error) {
	if !fi.Mode().IsDir() {
		return nil, errNotDirectory
	}

	v := make([]os.FileInfo, len(fi.fileInfos))
	for i, fi := range fi.fileInfos {
		v[i] = fi
	}
	return v, nil
}

type fileReader struct {
	name     string // the name used to open
	fileInfo *fileInfo
	reader   io.ReadCloser
	file     *os.File
	closed   bool
	readdir  []os.FileInfo
}

func (f *fileReader) Close() error {
	var errs []error
	if f.reader != nil {
		err := f.reader.Close()
		errs = append(errs, err)
	}
	var tempFile string
	if f.file != nil {
		tempFile = f.file.Name()
		err := f.file.Close()
		errs = append(errs, err)
	}
	if tempFile != "" {
		err := os.Remove(tempFile)
		errs = append(errs, err)
	}

	f.closed = true

	for _, err := range errs {
		if err != nil {
			return f.pathError("Close", err)
		}
	}
	return nil
}

func (f *fileReader) Read(p []byte) (n int, err error) {
	if f.closed {
		return 0, f.pathError("Read", errFileClosed)
	}
	if f.file != nil {
		return f.file.Read(p)
	}
	if f.reader == nil {
		f.reader, err = f.fileInfo.zipFile.Open()
		if err != nil {
			return 0, err
		}
	}
	return f.reader.Read(p)
}

func (f *fileReader) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return 0, f.pathError("Seek", errFileClosed)
	}
	if f.reader != nil {
		if err := f.reader.Close(); err != nil {
			return 0, err
		}
	}
	if f.file == nil && offset == 0 && whence == 0 {
		var err error
		f.reader, err = f.fileInfo.zipFile.Open()
		return 0, err
	}
	if err := f.createTempFile(); err != nil {
		return 0, err
	}
	return f.file.Seek(offset, whence)
}

func (f *fileReader) Readdir(count int) ([]os.FileInfo, error) {
	var err error
	var osFileInfos []os.FileInfo
	if count > 0 {
		if f.readdir == nil {
			f.readdir, err = f.fileInfo.readdir()
			if err != nil {
				return nil, f.pathError("Readdir", err)
			}
		}
		if len(f.readdir) >= count {
			osFileInfos = f.readdir[0:count]
			f.readdir = f.readdir[count:]
		} else {
			osFileInfos = f.readdir
			f.readdir = nil
			err = io.EOF
		}
	} else {
		osFileInfos, err = f.fileInfo.readdir()
		if err != nil {
			return nil, f.pathError("Readdir", err)
		}
	}
	return osFileInfos, err
}

func (f *fileReader) Stat() (os.FileInfo, error) {
	return f.fileInfo, nil
}

func (f *fileReader) createTempFile() error {
	if f.reader != nil {
		if err := f.reader.Close(); err != nil {
			return err
		}
		f.reader = nil
	}
	if f.file == nil {
		// Open a file that contains the contents of the zip file.
		osFile, err := createTempFile(f.fileInfo.zipFile)
		if err != nil {
			return err
		}
		f.file = osFile
	}
	return nil
}

func (f *fileReader) pathError(op string, err error) error {
	return &os.PathError{
		Op:   op,
		Path: f.name,
		Err:  err,
	}
}

// createTempFile creates a temporary file with the contents of the
// zip file. Used to implement io.Seeker interface.
func createTempFile(f *zip.File) (*os.File, error) {
	reader, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	tempFile, err := ioutil.TempFile("", "zipfs")
	if err != nil {
		return nil, err
	}

	_, err = io.Copy(tempFile, reader)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return nil, err
	}
	_, err = tempFile.Seek(0, os.SEEK_SET)
	if err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return nil, err
	}
	return tempFile, nil
}
