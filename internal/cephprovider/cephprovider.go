package cephprovider

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SoftwareDefinedBuildings/btrdb/bte"
	"github.com/SoftwareDefinedBuildings/btrdb/internal/bprovider"
	"github.com/SoftwareDefinedBuildings/btrdb/internal/configprovider"
	"github.com/ceph/go-ceph/rados"
	"github.com/huichen/murmur"
	logging "github.com/op/go-logging"
)

var logger *logging.Logger

func init() {
	logger = logging.MustGetLogger("log")
}

const NUM_RHANDLES = 16
const NUM_WHANDLES = 16

//We know we won't get any addresses here, because this is the relocation base as well
const METADATA_BASE = 0xFF00000000000000

//4096 blocks per addr lock
const ADDR_LOCK_SIZE = 0x1000000000
const ADDR_OBJ_SIZE = 0x0001000000

//Just over the DBSIZE
const MAX_EXPECTED_OBJECT_SIZE = 20485

//The number of RADOS blocks to cache (up to 16MB each, probably only 1.6MB each)
const RADOS_CACHE_SIZE = NUM_RHANDLES * 2

const OFFSET_MASK = 0xFFFFFF
const R_CHUNKSIZE = 1 << 20

//This is how many uuid/address pairs we will keep to facilitate appending to segments
//instead of creating new ones.
const WORTH_CACHING = OFFSET_MASK - MAX_EXPECTED_OBJECT_SIZE
const SEGCACHE_SIZE = 1024

// 1MB for write cache, I doubt we will ever hit this tbh
const WCACHE_SIZE = 1 << 20

// Makes 16MB for 16B sblocks
const SBLOCK_CHUNK_SHIFT = 20
const SBLOCK_CHUNK_MASK = 0xFFFFF
const SBLOCKS_PER_CHUNK = 1 << SBLOCK_CHUNK_SHIFT
const SBLOCK_SIZE = 16

var provided_rh int64

func UUIDSliceToArr(id []byte) [16]byte {
	rv := [16]byte{}
	copy(rv[:], id)
	return rv
}

type CephSegment struct {
	h           *rados.IOContext
	sp          *CephStorageProvider
	ptr         uint64
	naddr       uint64
	base        uint64 //Not the same as the provider's base
	warrs       [][]byte
	uid         [16]byte
	wcache      []byte
	wcache_base uint64
	hi          int //write handle index
}

type chunkreqindex struct {
	UUID [16]byte
	Addr uint64
}

type CephStorageProvider struct {
	rh           []*rados.IOContext
	conn         *rados.Conn
	rhidx        chan int
	rhidx_ret    chan int
	rh_avail     []bool
	wh           []*rados.IOContext
	whidx        chan int
	whidx_ret    chan int
	wh_avail     []bool
	ptr          uint64
	alloc        chan uint64
	segaddrcache map[[16]byte]uint64
	segcachelock sync.Mutex

	chunklock sync.Mutex
	chunkgate map[chunkreqindex][]chan []byte

	rcache *CephCache

	dataPool string
	hotPool  string

	cfg configprovider.Configuration

	annotationMu sync.Mutex
}

//Returns the address of the first free word in the segment when it was locked
func (seg *CephSegment) BaseAddress() uint64 {
	return seg.base
}

//Unlocks the segment for the StorageProvider to give to other consumers
//Implies a flush
func (seg *CephSegment) Unlock() {
	seg.flushWrite()
	seg.sp.whidx_ret <- seg.hi
	seg.warrs = nil
	if (seg.naddr & OFFSET_MASK) < WORTH_CACHING {
		seg.sp.segcachelock.Lock()
		seg.sp.pruneSegCache()
		seg.sp.segaddrcache[seg.uid] = seg.naddr
		seg.sp.segcachelock.Unlock()
	}

}

func (seg *CephSegment) flushWrite() {
	if len(seg.wcache) == 0 {
		return
	}
	address := seg.wcache_base
	aa := address >> 24
	oid := fmt.Sprintf("%032x%010x", seg.uid, aa)
	offset := address & 0xFFFFFF
	seg.h.Write(oid, seg.wcache, offset)

	for i := 0; i < len(seg.wcache); i += R_CHUNKSIZE {
		seg.sp.rcache.cacheInvalidate((uint64(i) + seg.wcache_base) & R_ADDRMASK)
	}
	//The C code does not finish immediately, so we need to keep a reference to the old
	//wcache array until the segment is unlocked
	seg.warrs = append(seg.warrs, seg.wcache)
	seg.wcache = make([]byte, 0, WCACHE_SIZE)
	seg.wcache_base = seg.naddr

}

var totalbytes int64

//Writes a slice to the segment, returns immediately
//Returns nil if op is OK, otherwise ErrNoSpace or ErrInvalidArgument
//It is up to the implementer to work out how to report no space immediately
//The uint64 is the address to be used for the next write
func (seg *CephSegment) Write(uuid []byte, address uint64, data []byte) (uint64, error) {
	atomic.AddInt64(&totalbytes, int64(len(data)))
	//We don't put written blocks into the cache, because those will be
	//in the dblock cache much higher up.
	if address != seg.naddr {
		logger.Panic("Non-sequential write")
	}

	if len(seg.wcache)+len(data)+2 > cap(seg.wcache) {
		seg.flushWrite()
	}

	base := len(seg.wcache)
	seg.wcache = seg.wcache[:base+2]
	seg.wcache[base] = byte(len(data))
	seg.wcache[base+1] = byte(len(data) >> 8)
	seg.wcache = append(seg.wcache, data...)

	naddr := address + uint64(len(data)+2)

	//OLD NOTE:
	//Note that it is ok for an object to "go past the end of the allocation". Naddr could be one byte before
	//the end of the allocation for example. This is not a problem as we never address anything except the
	//start of an object. This is why we do not add the object max size here
	//NEW NOTE:
	//We cannot go past the end of the allocation anymore because it would break the read cache
	if ((naddr + MAX_EXPECTED_OBJECT_SIZE + 2) >> 24) != (address >> 24) {
		//We are gonna need a new object addr
		naddr = <-seg.sp.alloc
		seg.naddr = naddr
		seg.flushWrite()
		return naddr, nil
	}
	seg.naddr = naddr

	return naddr, nil
}

//Block until all writes are complete. Note this does not imply a flush of the underlying files.
func (seg *CephSegment) Flush() {
	//Not sure we need to do stuff here, we can do it in unlock
}

//Must be called with the cache lock held
func (sp *CephStorageProvider) pruneSegCache() {
	//This is extremely rare, so its best to handle it simply
	//If we drop the cache, we will get one shortsized object per stream,
	//and it won't necessarily be _very_ short.
	if len(sp.segaddrcache) >= SEGCACHE_SIZE {
		sp.segaddrcache = make(map[[16]byte]uint64, SEGCACHE_SIZE)
	}
}

func (sp *CephStorageProvider) provideReadHandles() {
	for {
		//Read all returned read handles
	ldretfir:
		for {
			select {
			case fi := <-sp.rhidx_ret:
				sp.rh_avail[fi] = true
			default:
				break ldretfir
			}
		}

		found := false
		for i := 0; i < NUM_RHANDLES; i++ {
			if sp.rh_avail[i] {
				sp.rhidx <- i
				provided_rh += 1
				sp.rh_avail[i] = false
				found = true
			}
		}
		//If we didn't find one, do a blocking read
		if !found {
			idx := <-sp.rhidx_ret
			sp.rh_avail[idx] = true
		}
	}
}

func (sp *CephStorageProvider) provideWriteHandles() {
	for {
		//Read all returned write handles
	ldretfiw:
		for {
			select {
			case fi := <-sp.whidx_ret:
				sp.wh_avail[fi] = true
			default:
				break ldretfiw
			}
		}

		found := false
		for i := 0; i < NUM_WHANDLES; i++ {
			if sp.wh_avail[i] {
				sp.whidx <- i
				sp.wh_avail[i] = false
				found = true
			}
		}
		//If we didn't find one, do a blocking read
		if !found {
			idx := <-sp.whidx_ret
			sp.wh_avail[idx] = true
		}
	}
}

func (sp *CephStorageProvider) provideAllocs() {
	base := sp.ptr
	for {
		sp.alloc <- sp.ptr
		sp.ptr += ADDR_OBJ_SIZE
		if sp.ptr >= base+ADDR_LOCK_SIZE {
			sp.ptr = sp.obtainBaseAddress()
			base = sp.ptr
		}
	}
}

func (sp *CephStorageProvider) GetRH() int {
	select {
	case h := <-sp.rhidx:
		return h
	case <-time.After(10 * time.Second):
		panic(fmt.Sprintf("gottem %d", provided_rh))
	}
}
func (sp *CephStorageProvider) obtainBaseAddress() uint64 {
	addr := make([]byte, 8)
	hi := <-sp.rhidx
	h := sp.rh[hi]
	h.LockExclusive("allocator", "alloc_lock", "main", "alloc", 5*time.Second, nil)
	c, err := h.Read("allocator", addr, 0)
	if err != nil || c != 8 {
		h.Unlock("allocator", "alloc_lock", "main")
		sp.rhidx_ret <- hi
		return 0
	}
	le := binary.LittleEndian.Uint64(addr)
	ne := le + ADDR_LOCK_SIZE
	binary.LittleEndian.PutUint64(addr, ne)
	err = h.WriteFull("allocator", addr)
	if err != nil {
		panic("b")
	}
	h.Unlock("allocator", "alloc_lock", "main")
	sp.rhidx_ret <- hi
	return le
}

//Called at startup of a normal run
func (sp *CephStorageProvider) Initialize(cfg configprovider.Configuration) {
	//Allocate caches
	go func() {
		for {
			time.Sleep(1 * time.Second)
			logger.Infof("rawlp[%s %s=%d,%s=%d]", "cachegood", "actual", atomic.LoadInt64(&actualread), "used", atomic.LoadInt64(&readused))
		}
	}()
	sp.cfg = cfg
	sp.rcache = &CephCache{}
	cachesz := cfg.RadosReadCache()
	if cachesz < 40 {
		cachesz = 40 //one per read handle: 40MB
	}
	sp.rcache.initCache(uint64(cachesz))
	conn, err := rados.NewConn()
	if err != nil {
		logger.Panicf("Could not initialize ceph storage: %v", err)
	}
	err = conn.ReadConfigFile(cfg.StorageCephConf())
	if err != nil {
		logger.Panicf("Could not read ceph config: %v", err)
	}
	err = conn.Connect()
	if err != nil {
		logger.Panicf("Could not initialize ceph storage: %v", err)
	}
	sp.conn = conn
	sp.dataPool = cfg.StorageCephDataPool()
	sp.hotPool = cfg.StorageCephHotPool()

	sp.rh = make([]*rados.IOContext, NUM_RHANDLES)
	sp.rh_avail = make([]bool, NUM_RHANDLES)
	sp.rhidx = make(chan int, NUM_RHANDLES+1)
	sp.rhidx_ret = make(chan int, NUM_RHANDLES+1)
	sp.wh = make([]*rados.IOContext, NUM_RHANDLES)
	sp.wh_avail = make([]bool, NUM_WHANDLES)
	sp.whidx = make(chan int, NUM_WHANDLES+1)
	sp.whidx_ret = make(chan int, NUM_WHANDLES+1)
	sp.alloc = make(chan uint64, 128)
	sp.segaddrcache = make(map[[16]byte]uint64, SEGCACHE_SIZE)
	sp.chunkgate = make(map[chunkreqindex][]chan []byte)

	for i := 0; i < NUM_RHANDLES; i++ {
		sp.rh_avail[i] = true
		h, err := conn.OpenIOContext(sp.dataPool)
		if err != nil {
			logger.Panicf("Could not open CEPH: %v", err)
		}
		sp.rh[i] = h
	}

	for i := 0; i < NUM_WHANDLES; i++ {
		sp.wh_avail[i] = true
		h, err := conn.OpenIOContext(sp.dataPool)
		if err != nil {
			logger.Panicf("Could not open CEPH: %v", err)
		}
		sp.wh[i] = h
	}

	//Start serving read handles
	go sp.provideReadHandles()
	go sp.provideWriteHandles()
	//Obtain base address
	sp.ptr = sp.obtainBaseAddress()
	if sp.ptr == 0 {
		logger.Panic("Could not read allocator! DB not created properly?")
	}
	logger.Infof("Base address obtained as 0x%016x", sp.ptr)

	//Start providing address allocations
	go sp.provideAllocs()

}

//Called to create the database for the first time
//This doesn't lock, but nobody else would be trying to do the same thing at
//the same time, so...
func (sp *CephStorageProvider) CreateDatabase(cfg configprovider.Configuration) error {
	cephpool := cfg.StorageCephDataPool()
	cephconf := cfg.StorageCephConf()
	conn, err := rados.NewConn()
	if err != nil {
		panic(err)
	}
	err = conn.ReadConfigFile(cephconf)
	if err != nil {
		logger.Panicf("Could not read ceph config: %v", err)
	}
	fmt.Printf("reading ceph config: %s pool %s ", cephconf, cephpool)
	err = conn.Connect()
	if err != nil {
		logger.Panicf("Could not initialize ceph storage (likely a ceph.conf error): %v", err)
	}

	h, err := conn.OpenIOContext(cephpool)
	if err != nil {
		logger.Panicf("Could not create the ceph allocator context: %v", err)
	}
	addr := uint64(0x1000000)
	baddr := make([]byte, 8)
	binary.LittleEndian.PutUint64(baddr, addr)
	err = h.WriteFull("allocator", baddr)
	if err != nil {
		logger.Panicf("Could not create the ceph allocator handle: %v", err)
	}
	h.Destroy()
	return nil
}

// Lock a segment, or block until a segment can be locked
// Returns a Segment struct
// Implicit unchecked assumption: you cannot lock more than one segment
// for a given uuid (without unlocking them in between). It will break
// segcache
func (sp *CephStorageProvider) LockSegment(uuid []byte) bprovider.Segment {
	rv := new(CephSegment)
	rv.sp = sp
	rv.hi = <-sp.whidx
	rv.h = sp.wh[rv.hi]
	rv.ptr = <-sp.alloc
	rv.uid = UUIDSliceToArr(uuid)
	rv.wcache = make([]byte, 0, WCACHE_SIZE)
	sp.segcachelock.Lock()
	cached_ptr, ok := sp.segaddrcache[rv.uid]
	if ok {
		delete(sp.segaddrcache, rv.uid)
	}
	sp.segcachelock.Unlock()
	//ok = false
	if ok {
		rv.base = cached_ptr
		rv.naddr = rv.base
	} else {
		rv.base = rv.ptr
		rv.naddr = rv.base
	}
	rv.wcache_base = rv.naddr
	//Although I don't know this for sure, I am concerned that when we pass the write array pointer to C
	//the Go GC may free it before C is done. I prevent this by pinning all the written arrays, which get
	//deref'd after the segment is unlocked
	rv.warrs = make([][]byte, 0, 64)
	return rv
}

func (sp *CephStorageProvider) rawObtainChunk(uuid []byte, address uint64) []byte {
	chunk := sp.rcache.cacheGet(address)
	if chunk == nil {
		chunk = sp.rcache.getBlank()
		rhidx := sp.GetRH()
		aa := address >> 24
		oid := fmt.Sprintf("%032x%010x", uuid, aa)
		offset := address & 0xFFFFFF
		rc, err := sp.rh[rhidx].Read(oid, chunk, offset)
		atomic.AddInt64(&actualread, int64(rc))
		if err != nil {
			logger.Panicf("ceph error: %v", err)
		}
		chunk = chunk[0:rc]
		sp.rhidx_ret <- rhidx
		sp.rcache.cachePut(address, chunk)
	}
	return chunk
}

func (sp *CephStorageProvider) obtainChunk(uuid []byte, address uint64) []byte {
	chunk := sp.rcache.cacheGet(address)
	if chunk != nil {
		return chunk
	}
	index := chunkreqindex{UUID: UUIDSliceToArr(uuid), Addr: address}
	rvc := make(chan []byte, 1)
	sp.chunklock.Lock()
	slc, ok := sp.chunkgate[index]
	if ok {
		sp.chunkgate[index] = append(slc, rvc)
		sp.chunklock.Unlock()
	} else {
		sp.chunkgate[index] = []chan []byte{rvc}
		sp.chunklock.Unlock()
		go func() {
			bslice := sp.rawObtainChunk(uuid, address)
			sp.chunklock.Lock()
			slc, ok := sp.chunkgate[index]
			if !ok {
				panic("inconsistency!!")
			}
			for _, chn := range slc {
				chn <- bslice
			}
			delete(sp.chunkgate, index)
			sp.chunklock.Unlock()
		}()
	}
	rv := <-rvc
	return rv
}

// Read the blob into the given buffer: direct read
/*
func (sp *CephStorageProvider) Read(uuid []byte, address uint64, buffer []byte) []byte {

	//Get a read handle
	rhidx := <-sp.rhidx
	if len(buffer) < MAX_EXPECTED_OBJECT_SIZE {
		logger.Panic("That doesn't seem safe")
	}
	rc, err := C.handle_read(sp.rh[rhidx], (*C.uint8_t)(unsafe.Pointer(&uuid[0])), C.uint64_t(address), (*C.char)(unsafe.Pointer(&buffer[0])), MAX_EXPECTED_OBJECT_SIZE)
	if err != nil {
		logger.Panic("CGO ERROR: %v", err)
	}
	sp.rhidx_ret <- rhidx
	ln := int(buffer[0]) + (int(buffer[1]) << 8)
	if int(rc) < ln+2 {
		//TODO this can happen, it is better to just go back a few superblocks
		logger.Panic("Short read")
	}
	return buffer[2 : ln+2]
}*/

var exl_lock sync.Mutex

// Read the blob into the given buffer
func (sp *CephStorageProvider) Read(uuid []byte, address uint64, buffer []byte) []byte {
	//Get the first chunk for this object:
	chunk1 := sp.obtainChunk(uuid, address&R_ADDRMASK)[address&R_OFFSETMASK:]
	var chunk2 []byte
	var ln int

	if len(chunk1) < 2 {
		//not even long enough for the prefix, must be one byte in the first chunk, one in teh second
		chunk2 = sp.obtainChunk(uuid, (address+R_CHUNKSIZE)&R_ADDRMASK)
		ln = int(chunk1[0]) + (int(chunk2[0]) << 8)
		chunk2 = chunk2[1:]
		chunk1 = chunk1[1:]
	} else {
		ln = int(chunk1[0]) + (int(chunk1[1]) << 8)
		chunk1 = chunk1[2:]
	}

	if (ln) > MAX_EXPECTED_OBJECT_SIZE {
		logger.Panic("WTUF: ", ln)
	}

	copied := 0
	if len(chunk1) > 0 {
		//We need some bytes from chunk1
		end := ln
		if len(chunk1) < ln {
			end = len(chunk1)
		}
		copied = copy(buffer, chunk1[:end])
	}
	if copied < ln {
		//We need some bytes from chunk2
		if chunk2 == nil {
			chunk2 = sp.obtainChunk(uuid, (address+R_CHUNKSIZE)&R_ADDRMASK)
		}
		copy(buffer[copied:], chunk2[:ln-copied])

	}
	if ln < 2 {
		logger.Panic("This is unexpected")
	}
	exl_lock.Lock()
	_, ok := excludemap[address]
	if !ok {
		excludemap[address] = true
		readused += int64(ln)
	}
	exl_lock.Unlock()
	return buffer[:ln]

}

// Read the given version of superblock into the buffer.
// mebbeh we want to cache this?
func (sp *CephStorageProvider) ReadSuperBlock(uuid []byte, version uint64, buffer []byte) []byte {
	chunk := version >> SBLOCK_CHUNK_SHIFT
	offset := (version & SBLOCK_CHUNK_MASK) * SBLOCK_SIZE
	oid := fmt.Sprintf("sb%032x%011x", uuid, chunk)
	hi := sp.GetRH()
	h := sp.rh[hi]
	br, err := h.Read(oid, buffer, offset)
	if br != SBLOCK_SIZE || err != nil {
		logger.Panicf("unexpected sb read rv: %v %v offset=%v oid=%s version=%d bl=%d", br, err, offset, oid, version, len(buffer))
	}
	sp.rhidx_ret <- hi
	return buffer
}

// Writes a superblock of the given version
// TODO I think the storage will need to chunk this, because sb logs of gigabytes are possible
func (sp *CephStorageProvider) WriteSuperBlock(uuid []byte, version uint64, buffer []byte) {
	chunk := version >> SBLOCK_CHUNK_SHIFT
	offset := (version & SBLOCK_CHUNK_MASK) * SBLOCK_SIZE
	oid := fmt.Sprintf("sb%032x%011x", uuid, chunk)
	hi := <-sp.whidx
	h := sp.wh[hi]
	err := h.Write(oid, buffer, offset)
	if err != nil {
		logger.Panicf("unexpected sb write rv: %v", err)
	}
	sp.whidx_ret <- hi
}

// Sets the version of a stream. If it is in the past, it is essentially a rollback,
// and although no space is freed, the consecutive version numbers can be reused
// note to self: you must make sure not to call ReadSuperBlock on versions higher
// than you get from GetStreamVersion because they might succeed
func (sp *CephStorageProvider) SetStreamVersion(uuid []byte, version uint64) {
	oid := fmt.Sprintf("meta%032x", uuid)
	hi := sp.GetRH()
	h := sp.rh[hi]
	data := make([]byte, 8)
	binary.LittleEndian.PutUint64(data, version)
	err := h.SetXattr(oid, "version", data)
	if err != nil {
		logger.Panicf("ceph error: %v", err)
	}
	sp.rhidx_ret <- hi
}

// Gets the version of a stream. Returns 0 if none exists.
func (sp *CephStorageProvider) GetStreamInfo(uuid []byte) (bprovider.Stream, uint64) {
	oid := fmt.Sprintf("meta%032x", uuid)
	hi := sp.GetRH()
	h := sp.rh[hi]

	rv, err := h.ListXattrs(oid)
	if err == rados.RadosErrorNotFound {
		sp.rhidx_ret <- hi
		return nil, 0
	}
	if err != nil {
		logger.Panicf("weird ceph error getting xattrs: %v", err)
	}
	vdata := rv["version"]
	tdata := rv["stream"]
	ver := binary.LittleEndian.Uint64(vdata)
	tparts := strings.SplitN(string(tdata), ";", 2)
	collection := tparts[0]

	tags := strings.Split(tparts[1], "@")
	if tparts[1] == "" {
		tags = []string{}
	} else {
		tags = tags[:len(tags)-1]
	}
	tmap := make(map[string]string)
	for i := 0; i < len(tags); i += 2 {
		tmap[tags[i]] = tags[i+1]
	}

	sp.rhidx_ret <- hi

	return &cephStream{collection: collection, uuid: uuid, tags: tmap}, ver
}

// Gets the version of a stream. Returns 0 if none exists.
func (sp *CephStorageProvider) GetStreamVersion(uuid []byte) uint64 {
	oid := fmt.Sprintf("meta%032x", uuid)
	hi := sp.GetRH()
	h := sp.rh[hi]

	data := make([]byte, 8)
	bc, err := h.GetXattr(oid, "version", data)
	if err == rados.RadosErrorNotFound {
		sp.rhidx_ret <- hi
		return 0
	}
	if err != nil || bc != 8 {
		logger.Panicf("weird ceph error getting xattrs: %v", err)
	}
	sp.rhidx_ret <- hi
	ver := binary.LittleEndian.Uint64(data)
	return ver
}

var collectionRegex = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,254}$`)
var keysRegex = collectionRegex
var valsRegex = regexp.MustCompile(`^[a-zA-Z0-9 .-]*$`)

func isValidCollection(c string) bool {
	return collectionRegex.MatchString(c)
}

func isValidTagKey(k string) bool {
	return keysRegex.MatchString(k)
}

func isValidTagValue(v string) bool {
	return valsRegex.MatchString(v)
}

// CreateStream makes a stream with the given uuid, collection and tags. Returns
// an error if the uuid already exists.
func (sp *CephStorageProvider) CreateStream(uuid []byte, collection string, tags map[string]string, annotation []byte) bte.BTE {
	if !isValidCollection(collection) {
		return bte.Err(bte.InvalidCollection, "Invalid collection name")
	}
	if !sp.cfg.(configprovider.ClusterConfiguration).WeHoldWriteLockFor(uuid) {
		return bte.Err(bte.WrongEndpoint, "Wrong endpoint for UUID")
	}
	if len(annotation) > bprovider.MaxAnnotationSize {
		return bte.Err(bte.AnnotationTooBig, "Annotation too big")
	}
	sp.annotationMu.Lock()
	defer sp.annotationMu.Unlock()

	aoid := fmt.Sprintf("ann%032x", uuid)

	for k, v := range tags {
		if !isValidTagKey(k) {
			return bte.Err(bte.InvalidTagKey, "Invalid tag key")
		}
		if !isValidTagValue(v) {
			return bte.Err(bte.InvalidTagValue, "Invalid tag value")
		}
	}

	oid := fmt.Sprintf("meta%032x", uuid)
	hi := sp.GetRH()
	h := sp.rh[hi]
	defer func() { sp.rhidx_ret <- hi }()
	data := make([]byte, 8)
	bc, err := h.GetXattr(oid, "version", data)
	if err == nil {
		return bte.Err(bte.StreamExists, "Stream already exists")
	} else if err != rados.RadosErrorNotFound {
		logger.Panicf("ceph error getting version xattr: %v %v", err, bc)
	}

	//Create the composite list of tag values and keys
	tl := make([]string, 0, len(tags))
	for k, v := range tags {
		tl = append(tl, fmt.Sprintf("%s@%s@", k, v))
	}
	//Sort it so there is a canonical order
	sort.Strings(tl)
	tlkey := strings.Join(tl, "")

	//Check if the stream in collection exists
	found := false
	same := false
	err = h.ListOmapValues("col."+collection, "", tlkey, 10, func(k string, v []byte) {
		found = true
		if bytes.Equal(v, uuid) {
			same = true
		}
	})
	//BUG(mpa) rados returns shitty error here, so just ignore it
	// if err != nil && err != rados.RadosErrorNotFound {
	// 	logger.Panicf("ceph error checking if stream exists: %v", err)
	// }
	if found {
		if same {
			return bte.Err(bte.SameStream, "A stream exists with the same uuid and tags")
		} else {
			return bte.Err(bte.AmbiguousStream, "A stream exists with intersecting tags")
		}
	}
	//Now create a stream entry in the collection
	err = h.SetOmap("col."+collection, map[string][]byte{tlkey: uuid})
	if err != nil {
		logger.Panicf("ceph error setting tag set: %v", err)
	}

	//Now create the annotation
	verann := make([]byte, len(annotation)+8)
	binary.LittleEndian.PutUint64(verann[:8], 1)
	copy(verann[8:], annotation)
	h.WriteFull(aoid, verann)

	//Now note that the collection exists
	hash := murmur.Murmur3([]byte(collection))
	partition := hash >> 24
	err = h.SetOmap(fmt.Sprintf("index.%02x", partition), map[string][]byte{collection: []byte{46}})
	if err != nil {
		logger.Panicf("ceph error setting col index: %v", err)
	}

	//Set the collection and tags on the uuid
	err = h.SetXattr(oid, "stream", []byte(fmt.Sprintf("%s;%s", collection, tlkey)))
	if err != nil {
		logger.Panicf("ceph error: %v", err)
	}

	//As a final step, initialize the stream to version 9
	binary.LittleEndian.PutUint64(data, bprovider.SpecialVersionCreated)
	err = h.SetXattr(oid, "version", data)
	if err != nil {
		logger.Panicf("ceph error: %v", err)
	}

	return nil
}

// ListCollections returns a list of collections beginning with prefix (which may be "")
// and starting from the given string. Only number many results
// will be returned. More can be obtained by re-calling ListCollections with
// a given startingFrom and number.
func (sp *CephStorageProvider) ListCollections(prefix string, startingFrom string, number int64) ([]string, bte.BTE) {
	if (prefix != "" && !isValidCollection(prefix)) || (startingFrom != "" && !isValidCollection(startingFrom)) {
		return nil, bte.Err(bte.InvalidCollection, "Invalid collection name")
	}
	if number < 1 {
		return nil, bte.Err(bte.InvalidLimit, "Limit must be > 0")
	}
	hi := sp.GetRH()
	h := sp.rh[hi]
	rv := []string{}
	var hash uint32
	if startingFrom != "" {
		hash = murmur.Murmur3([]byte(startingFrom))
	}
	partition := hash >> 24
	for {
		err := h.ListOmapValues(fmt.Sprintf("index.%02x", partition), startingFrom, prefix, number, func(key string, val []byte) {
			number--
			rv = append(rv, key)
		})
		//As usual, if the object doesn't exist, the error is just "i/o error"
		_ = err
		// if err != nil && err != rados.RadosErrorNotFound {
		// 	logger.Panicf("ceph error %v", err)
		// }
		startingFrom = ""
		partition++
		if partition > 255 || number == 0 {
			sp.rhidx_ret <- hi
			return rv, nil
		}
	}
}

func (sp *CephStorageProvider) SetStreamAnnotation(uuid []byte, aver uint64, ann []byte) bte.BTE {
	//We know that we are the only server that is accessing this uuid, so we can
	//avoid costly distributed locks. But we need to ensure that we do not conflict
	//with any other requests on the same server
	sp.annotationMu.Lock()
	defer sp.annotationMu.Unlock()

	oid := fmt.Sprintf("ann%032x", uuid)
	hi := sp.GetRH()
	h := sp.rh[hi]
	defer func() { sp.rhidx_ret <- hi }()

	dat := make([]byte, 8)
	bc, err := h.Read(oid, dat, 0)
	if err != nil {
		if err == rados.RadosErrorNotFound {
			return bte.Err(bte.NoSuchStream, "Stream does not exist")
		}
		//Not 404?
		logger.Panicf("Unexpected error retrieving annotation object uuid=%v err=%v", uuid, err)
	}
	if bc != 8 {
		logger.Panicf("Short read on annotation object uuid=%v bc=%d", uuid, bc)
	}
	existingAver := binary.LittleEndian.Uint64(dat)

	if existingAver != aver && aver != 0 {
		return bte.Err(bte.AnnotationVersionMismatch, fmt.Sprintf("Stream annotation version is %d, not %d", existingAver, aver))
	}
	nextAver := existingAver + 1
	payload := make([]byte, len(ann)+8)
	binary.LittleEndian.PutUint64(payload, nextAver)
	copy(payload[8:], ann)

	err = h.WriteFull(oid, payload)
	if err != nil {
		logger.Panicf("Could not write annotation %v", err)
	}
	return nil
}

// GetStreamAnnotation gets the annotation for a given stream
func (sp *CephStorageProvider) GetStreamAnnotation(uuid []byte) ([]byte, uint64, bte.BTE) {
	sp.annotationMu.Lock()
	defer sp.annotationMu.Unlock()

	oid := fmt.Sprintf("ann%032x", uuid)
	hi := sp.GetRH()
	h := sp.rh[hi]
	defer func() { sp.rhidx_ret <- hi }()
	rv := bytes.Buffer{}
	var off uint64
	seg := make([]byte, 128*1024)
	for {
		num, err := h.Read(oid, seg, off)
		rv.Write(seg[:num])
		if err != nil {
			break
		}
		if num < 128*1024 {
			break
		}
		off += uint64(num)
	}
	rvarr := rv.Bytes()
	ver := binary.LittleEndian.Uint64(rvarr[:8])
	return rvarr[8:], ver, nil
}

// ListStreams lists all the streams within a collection. If tags are specified
// then streams are only returned if they have that tag, and the value equals
// the value passed.
func (sp *CephStorageProvider) ListStreams(collection string, partial bool, tags map[string]string) ([]bprovider.Stream, bte.BTE) {
	if !isValidCollection(collection) {
		return nil, bte.Err(bte.InvalidCollection, "Invalid collection name")
	}
	for k, v := range tags {
		if !isValidTagKey(k) {
			return nil, bte.Err(bte.InvalidTagKey, "Invalid tag key")
		}
		if !isValidTagValue(v) {
			return nil, bte.Err(bte.InvalidTagValue, "Invalid tag value")
		}
	}
	hi := sp.GetRH()
	h := sp.rh[hi]
	defer func() { sp.rhidx_ret <- hi }()
	if partial {
		rv := []bprovider.Stream{}
		err := h.ListOmapValues("col."+collection, "", "", 1000000, func(key string, val []byte) {
			tags := strings.Split(key, "@")
			if key == "" {
				tags = []string{}
			} else {
				tags = tags[:len(tags)-1]
			}
			tmap := make(map[string]string)
			if len(tags)%2 != 0 {
				logger.Panicf("Odd tags: %s", key)
			}
			for i := 0; i < len(tags); i += 2 {
				tmap[tags[i]] = tags[i+1]
			}
			uuid := val[:16]
			rv = append(rv, &cephStream{uuid: uuid, collection: collection, tags: tmap})
		})
		if err != nil && err != rados.RadosErrorNotFound {
			logger.Panicf("got error %v", err)
		}
		if err == rados.RadosErrorNotFound {
			return nil, bte.Err(bte.NoSuchStream, "Collection not found")
		}
		return rv, nil
	} else {
		tl := make([]string, 0, len(tags))
		for k, v := range tags {
			tl = append(tl, fmt.Sprintf("%s@%s@", k, v))
		}
		//Sort it so there is a canonical order
		sort.Strings(tl)
		tlkey := strings.Join(tl, "")
		//Get UUID
		rv, err := h.GetOmapValues("col."+collection, "", tlkey, 10)
		if err == rados.RadosErrorNotFound || len(rv) == 0 {
			return nil, bte.Err(bte.NoSuchStream, "Could not find stream")
		}
		if len(rv) > 1 {
			return nil, bte.Err(bte.AmbiguousTags, "Tags do not uniquely identify a stream")
		}
		srv := []bprovider.Stream{}
		for k, val := range rv {
			tags := strings.Split(k, "@")
			if k == "" {
				tags = []string{}
			} else {
				tags = tags[:len(tags)-1]
			}
			tmap := make(map[string]string)
			for i := 0; i < len(tags); i += 2 {
				tmap[tags[i]] = tags[i+1]
			}
			uuid := val[:16]
			srv = append(srv, &cephStream{uuid: uuid, collection: collection, tags: tmap})
			break
		}
		return srv, nil
	}

}

type cephStream struct {
	uuid       []byte
	collection string
	tags       map[string]string
}

func (cs *cephStream) UUID() []byte {
	return cs.uuid
}

func (cs *cephStream) Collection() string {
	return cs.collection
}

func (cs *cephStream) Tags() map[string]string {
	return cs.tags
}
