// Copyright 2017 The go-hep Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rootio

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
)

// Reader is the rootio interface to interact with ROOT
// files open in read-only mode.
type Reader interface {
	io.Reader
	io.ReaderAt
	io.Seeker
	io.Closer
}

// Writer is the rootio interface to interact with ROOT
// files open in write-only mode.
type Writer interface {
	io.Writer
	io.WriterAt
	io.Seeker
	io.Closer
}

// A ROOT file is a suite of consecutive data records (TKey's) with
// the following format (see also the TKey class). If the key is
// located past the 32 bit file limit (> 2 GB) then some fields will
// be 8 instead of 4 bytes:
//    1->4            Nbytes    = Length of compressed object (in bytes)
//    5->6            Version   = TKey version identifier
//    7->10           ObjLen    = Length of uncompressed object
//    11->14          Datime    = Date and time when object was written to file
//    15->16          KeyLen    = Length of the key structure (in bytes)
//    17->18          Cycle     = Cycle of key
//    19->22 [19->26] SeekKey   = Pointer to record itself (consistency check)
//    23->26 [27->34] SeekPdir  = Pointer to directory header
//    27->27 [35->35] lname     = Number of bytes in the class name
//    28->.. [36->..] ClassName = Object Class Name
//    ..->..          lname     = Number of bytes in the object name
//    ..->..          Name      = lName bytes with the name of the object
//    ..->..          lTitle    = Number of bytes in the object title
//    ..->..          Title     = Title of the object
//    ----->          DATA      = Data bytes associated to the object
//
// The first data record starts at byte fBEGIN (currently set to kBEGIN).
// Bytes 1->kBEGIN contain the file description, when fVersion >= 1000000
// it is a large file (> 2 GB) and the offsets will be 8 bytes long and
// fUnits will be set to 8:
//    1->4            "root"      = Root file identifier
//    5->8            fVersion    = File format version
//    9->12           fBEGIN      = Pointer to first data record
//    13->16 [13->20] fEND        = Pointer to first free word at the EOF
//    17->20 [21->28] fSeekFree   = Pointer to FREE data record
//    21->24 [29->32] fNbytesFree = Number of bytes in FREE data record
//    25->28 [33->36] nfree       = Number of free data records
//    29->32 [37->40] fNbytesName = Number of bytes in TNamed at creation time
//    33->33 [41->41] fUnits      = Number of bytes for file pointers
//    34->37 [42->45] fCompress   = Compression level and algorithm
//    38->41 [46->53] fSeekInfo   = Pointer to TStreamerInfo record
//    42->45 [54->57] fNbytesInfo = Number of bytes in TStreamerInfo record
//    46->63 [58->75] fUUID       = Universal Unique ID
type File struct {
	r      Reader
	w      Writer
	seeker io.Seeker
	closer io.Closer

	id string //non-root, identifies filename, etc.

	version int32
	begin   int64

	// Remainder of record is variable length, 4 or 8 bytes per pointer
	end         int64
	seekfree    int64 // first available record
	nbytesfree  int32 // total bytes available
	nfree       int32 // total free bytes
	nbytesname  int32 // number of bytes in TNamed at creation time
	units       byte
	compression int32
	seekinfo    int64 // pointer to TStreamerInfo
	nbytesinfo  int32 // sizeof(TStreamerInfo)
	uuid        [18]byte

	dir    tdirectory // root directory of this file
	siKey  Key
	sinfos []StreamerInfo

	blocks blocks // blocks is a list of free blocks in a ROOT file.
}

// Open opens the named ROOT file for reading. If successful, methods on the
// returned file can be used for reading; the associated file descriptor
// has mode os.O_RDONLY.
func Open(path string) (*File, error) {
	fd, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("rootio: unable to open %q (%q)", path, err.Error())
	}

	f := &File{
		r:      fd,
		seeker: fd,
		closer: fd,
		id:     path,
	}
	f.dir = tdirectory{file: f}

	err = f.readHeader()
	if err != nil {
		return nil, fmt.Errorf("rootio: failed to read header %q: %v", path, err)
	}

	return f, nil
}

// NewReader creates a new ROOT file reader.
func NewReader(r Reader, name string) (*File, error) {
	f := &File{
		r:      r,
		seeker: r,
		closer: r,
		id:     name,
	}
	f.dir = tdirectory{file: f}

	err := f.readHeader()
	if err != nil {
		return nil, fmt.Errorf("rootio: failed to read header: %v", err)
	}

	return f, nil
}

// Create creates the named ROOT file for writing.
func Create(name string) (*File, error) {
	fd, err := os.Create(name)
	if err != nil {
		return nil, fmt.Errorf("rootio: unable to create %q (%q)", name, err.Error())
	}

	f := &File{
		w:      fd,
		seeker: fd,
		closer: fd,
		id:     name,
	}
	f.dir = tdirectory{named: tnamed{name: name}, file: f}

	err = f.writeHeader()
	if err != nil {
		return nil, fmt.Errorf("rootio: failed to write header %q: %v", name, err)
	}

	return f, nil
}

// Read implements io.Reader
func (f *File) Read(p []byte) (int, error) {
	return f.r.Read(p)
}

// ReadAt implements io.ReaderAt
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	return f.r.ReadAt(p, off)
}

// Seek implements io.Seeker
func (f *File) Seek(offset int64, whence int) (int64, error) {
	return f.seeker.Seek(offset, whence)
}

// Version returns the ROOT version this file was created with.
func (f *File) Version() int {
	return int(f.version)
}

func (f *File) readHeader() error {

	buf := make([]byte, 64)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return err
	}

	r := NewRBuffer(buf, nil, 0)

	// Header

	var magic [4]byte
	if _, err := io.ReadFull(r.r, magic[:]); err != nil || string(magic[:]) != "root" {
		if err != nil {
			return fmt.Errorf("rootio: failed to read ROOT file magic header: %v", err)
		}
		return fmt.Errorf("rootio: %q is not a root file", f.id)
	}

	f.version = r.ReadI32()
	f.begin = int64(r.ReadI32())
	if f.version < 1000000 { // small file
		f.end = int64(r.ReadI32())
		f.seekfree = int64(r.ReadI32())
		f.nbytesfree = r.ReadI32()
		f.nfree = r.ReadI32()
		f.nbytesname = r.ReadI32()
		f.units = r.ReadU8()
		f.compression = r.ReadI32()
		f.seekinfo = int64(r.ReadI32())
		f.nbytesinfo = r.ReadI32()
	} else { // large files
		f.end = r.ReadI64()
		f.seekfree = r.ReadI64()
		f.nbytesfree = r.ReadI32()
		f.nfree = r.ReadI32()
		f.nbytesname = r.ReadI32()
		f.units = r.ReadU8()
		f.compression = r.ReadI32()
		f.seekinfo = r.ReadI64()
		f.nbytesinfo = r.ReadI32()
	}
	f.version %= 1000000

	if _, err := io.ReadFull(r.r, f.uuid[:]); err != nil || r.Err() != nil {
		if err != nil {
			return fmt.Errorf("rootio: failed to read ROOT's UUID file: %v", err)
		}
		return r.Err()
	}

	var err error

	err = f.dir.readDirInfo()
	if err != nil {
		return fmt.Errorf("rootio: failed to read ROOT directory infos: %v", err)
	}

	err = f.readStreamerInfo()
	if err != nil {
		return fmt.Errorf("rootio: failed to read ROOT streamer infos: %v", err)
	}

	err = f.dir.readKeys()
	if err != nil {
		return fmt.Errorf("rootio: failed to read ROOT file keys: %v", err)
	}

	return nil
}

func (f *File) writeHeader() error {
	_, err := f.w.Write([]byte("root"))
	if err != nil {
		return err
	}

	err = binary.Write(f.w, binary.BigEndian, uint32(rootVersion))
	if err != nil {
		return err
	}

	f.begin = kBEGIN
	f.end = kBEGIN
	f.blocks = append(f.blocks, block{f.begin, kStartBigFile})

	namelen := tstringSizeof(f.Name()) + tstringSizeof(f.Title())
	nbytes := int32(namelen) + int32(f.dir.recordSize(rootVersion))
	k := newHeaderKey(f.Name(), f.Title(), f.Class(), f, nbytes)
	f.nbytesname = k.keylen + int32(namelen)
	f.dir.seekdir = k.seekkey
	f.seekfree = 0
	f.nbytesfree = 0
	// ->> TFile::WriteHeader()
	f.end = f.blocks[len(f.blocks)-1].first
	f.nfree = int32(len(f.blocks))
	f.units = 4
	if f.version < 1000000 && f.end > kStartBigFile {
		f.version += 1000000
		f.units = 8
	}
	f.compression = 1
	//f.seekinfo = 216
	//f.nbytesinfo = 85

	if true {
		log.Printf("end=       %4d | 403", f.end)
		// f.end = 403
		log.Printf("seekfree=  %4d | 349", f.seekfree)
		log.Printf("nbytesfree=%4d |  54", f.nbytesfree)
		log.Printf("nfree=     %4d |  ??", f.nfree)
		log.Printf("nbytesname=%4d |  ??", f.nbytesname)
		log.Printf("units=     %4d |  ??", f.units)
		log.Printf("compr=     %4d |   1", f.compression)
		log.Printf("seekinfo=  %4d | 215", f.seekinfo)
		log.Printf("nbytesinfo=%4d |  85", f.nbytesinfo)
	}

	w := NewWBufferFrom(f.w, nil, 0)
	w.WriteI32(int32(f.begin))
	if f.version < 1000000 { // small file
		w.WriteI32(int32(f.end))
		w.WriteI32(int32(f.seekfree))
		w.WriteI32(f.nbytesfree)
		w.WriteI32(f.nfree)
		w.WriteI32(f.nbytesname)
		w.WriteU8(f.units)
		w.WriteI32(f.compression)
		w.WriteI32(int32(f.seekinfo))
		w.WriteI32(f.nbytesinfo)
	} else { // large files
		w.WriteI64(f.end)
		w.WriteI64(f.seekfree)
		w.WriteI32(f.nbytesfree)
		w.WriteI32(f.nfree)
		w.WriteI32(f.nbytesname)
		w.WriteU8(f.units)
		w.WriteI32(f.compression)
		w.WriteI64(f.seekinfo)
		w.WriteI32(f.nbytesinfo)
	}
	w.write(f.uuid[:])

	fmt.Printf("key=%v\n", k)

	// <<- TFile::WriteHeader
	wkey := NewWBuffer(k.buf, nil, 0)
	f.dir.named.MarshalROOT(wkey)
	f.dir.MarshalROOT(wkey)
	k.writeFile()

	return w.err
}

func (f *File) Map() {
	for _, k := range f.dir.keys {
		if k.class == "TBasket" {
			//b := k.AsBasket()
			fmt.Printf("%8s %60s %6v %6v %f\n", k.class, k.name, k.bytes-k.keylen, k.objlen, float64(k.objlen)/float64(k.bytes-k.keylen))
		} else {
			//println(k.classname, k.name, k.title)
			fmt.Printf("%8s %60s %6v %6v %f\n", k.class, k.name, k.bytes-k.keylen, k.objlen, float64(k.objlen)/float64(k.bytes-k.keylen))
		}
	}
}

func (f *File) Tell() int64 {
	where, err := f.Seek(0, ioSeekCurrent)
	if err != nil {
		panic(err)
	}
	return where
}

// Close closes the File, rendering it unusable for I/O.
// It returns an error, if any.
func (f *File) Close() error {
	for _, k := range f.dir.keys {
		k.f = nil
	}
	f.dir.keys = nil
	f.dir.file = nil
	return f.closer.Close()
}

// Keys returns the list of keys this File contains
func (f *File) Keys() []Key {
	return f.dir.keys
}

func (f *File) Name() string {
	return f.dir.Name()
}

func (f *File) Title() string {
	return f.dir.Title()
}

func (f *File) Class() string {
	return "TFile"
}

// readStreamerInfo reads the list of StreamerInfo from this file
func (f *File) readStreamerInfo() error {
	if f.seekinfo <= 0 || f.seekinfo >= f.end {
		return fmt.Errorf("rootio: invalid pointer to StreamerInfo (pos=%v end=%v)", f.seekinfo, f.end)

	}
	buf := make([]byte, int(f.nbytesinfo))
	nbytes, err := f.ReadAt(buf, f.seekinfo)
	if err != nil {
		return err
	}
	if nbytes != int(f.nbytesinfo) {
		return fmt.Errorf("rootio: requested [%v] bytes. read [%v] bytes from file", f.nbytesinfo, nbytes)
	}

	err = f.siKey.UnmarshalROOT(NewRBuffer(buf, nil, 0))
	f.siKey.f = f
	if err != nil {
		return err
	}

	objs := f.siKey.Value().(List)
	f.sinfos = make([]StreamerInfo, 0, objs.Len())
	for i := 0; i < objs.Len(); i++ {
		obj, ok := objs.At(i).(StreamerInfo)
		if !ok {
			continue
		}
		f.sinfos = append(f.sinfos, obj)
		streamers.add(obj)
	}
	return nil
}

// StreamerInfo returns the list of StreamerInfos of this file.
func (f *File) StreamerInfo() []StreamerInfo {
	return f.sinfos
}

// Get returns the object identified by namecycle
//   namecycle has the format name;cycle
//   name  = * is illegal, cycle = * is illegal
//   cycle = "" or cycle = 9999 ==> apply to a memory object
//
//   examples:
//     foo   : get object named foo in memory
//             if object is not in memory, try with highest cycle from file
//     foo;1 : get cycle 1 of foo on file
func (f *File) Get(namecycle string) (Object, error) {
	return f.dir.Get(namecycle)
}

// block describes a free block in a ROOT file.
type block struct {
	first int64
	last  int64
}

// blocks is a list of free blocks in a ROOT file.
type blocks []block

func (blks *blocks) add(first, last int64) int {
	for i := range *blks {
		blk := &(*blks)[i]
		if blk.last == first-1 {
			blk.last = last
			if i+1 >= len(*blks) {
				return i
			}
			next := &(*blks)[i+1]
			if next.first > last+1 {
				return i
			}
			blk.last = next.last
			(*blks) = append((*blks)[:i+1], (*blks)[i+2:]...)
			return i
		}
		if blk.first == last+1 {
			blk.first = first
			return i
		}
		if first < blk.first {
			free := block{first, last}
			*blks = append((*blks)[:i], append([]block{free}, (*blks)[i:]...)...)
			return i
		}
	}
	return -1
}

// best returns the best free block where to store nbytes.
func (blks blocks) best(nbytes int32) *block {
	var blk *block
	for i := range blks {
		cur := &blks[i]
		nleft := cur.last - cur.first + 1
		if nleft == int64(nbytes) {
			// found an exact match
			return cur
		}
		if nleft > int64(nbytes+3) {
			if blk == nil {
				blk = cur
			}
		}
	}

	// return first segment that can contain 'nbytes'
	if blk != nil {
		return blk
	}

	// try big file
	blk = &blks[len(blks)-1]
	blk.last += 1000000000
	return blk
}

func (blks *blocks) remove(blk *block) {
	i := -1
	for ii, bb := range *blks {
		if bb == *blk {
			i = ii
			break
		}
	}

	if i == -1 {
		panic("rootio: impossible")
	}

	*blks = append((*blks)[:i], (*blks)[i+1:]...)
}

var _ Object = (*File)(nil)
var _ Named = (*File)(nil)
var _ Directory = (*File)(nil)
