package shakti

import (
	"github.com/andy-kimball/arenaskl"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/shakti/cloudstore"
	"github.com/squareup/pranadb/shakti/cmn"
	"github.com/squareup/pranadb/shakti/datacontroller"
	"github.com/squareup/pranadb/shakti/iteration"
	"github.com/squareup/pranadb/shakti/mem"
	"github.com/squareup/pranadb/shakti/sst"
	"sync"
	"sync/atomic"
	"time"
)

type Shakti struct {
	dbID          uint64
	startStopLock sync.Mutex
	started       bool
	conf          cmn.Conf
	memtable      *mem.Memtable
	arena         *arenaskl.Arena
	cloudStore    cloudstore.Store
	controller    datacontroller.Controller
	TableCache    *sst.Cache
	mtLock        sync.RWMutex
	mtFlushChan   chan struct{}
	mtFlushQueue  []mtFlushEntry
	// We use a separate lock to protect the flush queue as we don't want removing first element from queue to block
	// writes to the memtable
	mtFlushQueueLock            common.SpinLock
	iterators                   map[*shaktiIterator]struct{}
	mtReplaceTimer              *time.Timer
	mtLastReplace               uint64
	mtMaxReplaceTime            uint64
	lastCommittedBatchSequences sync.Map
	stopWg                      sync.WaitGroup
}

func NewShakti(dbID uint64, store cloudstore.Store, registry datacontroller.Controller, conf cmn.Conf) *Shakti {
	arena := arenaskl.NewArena(uint32(conf.MemtableMaxSizeBytes))
	memtable := mem.NewMemtable(arena)
	return &Shakti{
		dbID:             dbID,
		conf:             conf,
		arena:            arena,
		memtable:         memtable,
		cloudStore:       store,
		controller:       registry,
		TableCache:       sst.NewTableCache(store),
		mtFlushChan:      make(chan struct{}, conf.MemtableFlushQueueMaxSize),
		iterators:        map[*shaktiIterator]struct{}{},
		mtMaxReplaceTime: uint64(conf.MemTableMaxReplaceTime),
	}
}

func (s *Shakti) Start() error {
	s.startStopLock.Lock()
	defer s.startStopLock.Unlock()
	s.started = true
	s.stopWg.Add(1)
	go s.mtFlushRunLoop()
	s.scheduleMtReplace()
	return nil
}

func (s *Shakti) Stop() error {
	s.startStopLock.Lock()
	defer s.startStopLock.Unlock()
	s.started = false
	if s.mtReplaceTimer != nil {
		s.mtReplaceTimer.Stop()
		s.mtReplaceTimer = nil
	}
	close(s.mtFlushChan)
	s.stopWg.Wait()
	return nil
}

func NewWriteBatch(processorID uint64, sequenceNum int64, batch *mem.Batch, completionFunc func(error) error) *WriteBatch {
	return &WriteBatch{
		ProcessorID:    processorID,
		SequenceNum:    sequenceNum,
		Batch:          batch,
		CompletionFunc: completionFunc,
	}
}

type WriteBatch struct {
	ProcessorID    uint64
	SequenceNum    int64
	Batch          *mem.Batch
	CompletionFunc func(error) error
}

//func (wb *WriteBatch) committed() {
//	wb.CompletionFunc()
//}

func (s *Shakti) Write(batch *WriteBatch) error {
	for {
		ok := s.checkDedupCache(batch)
		if !ok {
			// We have seen this batch before - ignore - this can happen on recovery after failure
			return nil
		}
		// Add dedup entry to batch
		s.putDedupEntry(batch)
		memtable, ok, err := s.doWrite(batch)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		// No more space left in memtable - swap writeIter out and replace writeIter with a new one and flush writeIter async
		if err := s.replaceMemtable(memtable); err != nil {
			return err
		}
	}
}

func (s *Shakti) putDedupEntry(batch *WriteBatch) {
	if s.conf.DisableBatchSequenceInsertion {
		return
	}
	// We add a row in the batch recording the sequence number of the batch
	key := s.createDedupKey(batch.ProcessorID)
	value := common.AppendUint64ToBufferLE(nil, uint64(batch.SequenceNum))
	// TODO: Note that putting this key in the batch will mean the common prefix for the batch will probably be just
	// app_id possibly making the batch encoded size largerl but maybe nothing to worry about
	batch.Batch.AddEntry(cmn.KV{
		Key:   key,
		Value: value,
	})
}

func (s *Shakti) createDedupKey(processorID uint64) []byte {
	key := cmn.EncodeKeyPrefix(nil, s.dbID, cmn.SystemTableDedupID, 0)
	key = common.AppendUint64ToBufferBE(key, uint64(processorID))
	return key
}

func (s *Shakti) LoadLastBatchSequence(processorID uint64) error {
	rangeStart := s.createDedupKey(processorID)

	rangeEnd := common.IncrementBytesBigEndian(rangeStart)
	iter, err := s.NewIterator(rangeStart, rangeEnd)
	if err != nil {
		return err
	}
	valid, err := iter.IsValid()
	if err != nil {
		return err
	}
	if valid {
		kv := iter.Current()
		lastSequence, _ := common.ReadUint64FromBufferLE(kv.Value, 0)
		s.lastCommittedBatchSequences.Store(processorID, int64(lastSequence))
	}
	return nil
}

func (s *Shakti) checkDedupCache(batch *WriteBatch) bool {
	if batch.SequenceNum == -1 {
		// -1 means don't check sequence number - used only in testing
		return true
	}
	// We check whether we have seen the sequence number from the same processor before
	o, ok := s.lastCommittedBatchSequences.Load(batch.ProcessorID)
	if ok {
		lastSeq := o.(int64) //nolint:forcetypeassert
		if batch.SequenceNum <= lastSeq {
			// Duplicate batch - we will ignore it
			return false
		}
	}
	s.lastCommittedBatchSequences.Store(batch.ProcessorID, batch.SequenceNum)
	return true
}

// Used in testing only
func (s *Shakti) forceReplaceMemtable() error {
	s.mtLock.Lock()
	defer s.mtLock.Unlock()
	return s.replaceMemtable0(s.memtable)
}

func (s *Shakti) replaceMemtable(memtable *mem.Memtable) error {
	s.mtLock.Lock()
	defer s.mtLock.Unlock()
	return s.replaceMemtable0(memtable)
}

func (s *Shakti) replaceMemtable0(memtable *mem.Memtable) error {
	// We do a check that it's the same memtable here under lock as writes are concurrent and two writes could
	// concurrently return full - we don't want to replace the mt more than once!
	if memtable == s.memtable {
		log.Debug("Adding memtable to flush queue and creating a new one")

		/*
			TODO adaptive memtable arena size
			The relationship between arena size and actual serialized SSTable size is complex due to:
			1. If the common key prefix is significant then the SSTable can be a lot smaller
			2. Index section
			3. Metadata section
			When SSTables are built, we can measure their size and automatically adjust arena size for the next memtable
			e.g. +- 5% if the SSTable size is far from the ideal size
		*/

		// TODO once a memtable has been fully flushed and removed from the flush queue and there are no more iterators
		// on it, we can reuse the arena o avoid creating new ones each time (i.e. create an arena pool)
		s.arena = arenaskl.NewArena(uint32(s.conf.MemtableMaxSizeBytes))
		s.memtable = mem.NewMemtable(s.arena)

		if err := s.updateIterators(s.memtable); err != nil {
			return err
		}

		s.mtFlushQueueLock.Lock()
		s.mtFlushQueue = append(s.mtFlushQueue, mtFlushEntry{
			memtable: memtable,
		})
		s.mtFlushQueueLock.Unlock()
		s.mtFlushChan <- struct{}{}
		s.mtLastReplace = common.NanoTime()
	}
	return nil
}

func (s *Shakti) updateIterators(mt *mem.Memtable) error {
	for iter := range s.iterators {
		rs, re, lastKey := iter.getRange()
		if lastKey != nil {
			rs = common.IncrementBytesBigEndian(lastKey)
		}
		mtIter := mt.NewIterator(rs, re)
		if err := iter.addNewMemtableIterator(mtIter); err != nil {
			return err
		}
	}
	return nil
}

func (s *Shakti) doWrite(batch *WriteBatch) (*mem.Memtable, bool, error) {
	s.mtLock.RLock()
	defer s.mtLock.RUnlock()
	mt := s.memtable
	ok, err := mt.Write(batch.Batch, batch.CompletionFunc)
	return mt, ok, err
}

func (s *Shakti) NewIterator(keyStart []byte, keyEnd []byte) (iteration.Iterator, error) {

	ids, err := s.controller.GetTableIDsForRange(keyStart, keyEnd, 10000) // TODO don't hardcode
	if err != nil {
		return nil, err
	}

	// TODO we should prevent very slow or stalled iterators from holding memtables or sstables in memory too long
	// we should detect if they are very slow, and close them if they are
	s.mtLock.RLock()
	defer s.mtLock.RUnlock()
	// We creating a merging iterator which merges from a set of potentially overlapping Memtables/SSTables in order
	// from newest to oldest
	iters := make([]iteration.Iterator, len(ids)+1+len(s.mtFlushQueue))
	pos := 0
	// First we add the current memtable
	iters[pos] = s.memtable.NewIterator(keyStart, keyEnd)
	pos++
	s.mtFlushQueueLock.Lock()
	// Then we add each memtable in the flush queue, in order from newest to oldest
	for i := len(s.mtFlushQueue) - 1; i >= 0; i-- {
		fe := s.mtFlushQueue[i]
		iters[pos] = fe.memtable.NewIterator(keyStart, keyEnd)
		pos++
	}
	s.mtFlushQueueLock.Unlock()

	// Then we add each flushed SSTable with overlapping keys from the controller. It's possible we might have the included
	// the same keys twice in a memtable from the flush queue which has been already flushed and one from the controller
	// This is ok as he later one (the sstable) will just be ignored in the iterator. However TODO we could detect
	// this and not add writeIter if this is the case
	for i, nonOverLapIDs := range ids {
		if len(nonOverLapIDs) == 1 {
			lazy, err := sst.NewLazySSTableIterator(nonOverLapIDs[0], s.TableCache, keyStart, keyEnd)
			if err != nil {
				return nil, err
			}
			if i+pos >= len(iters) {
				log.Println("foo")
			}
			iters[pos] = lazy
		} else {
			// TODO - instead of getting all table ids and constructing a chain iterator with potentially millions of
			// LazySSTableIterators (e.g. in the case the range is large and there is a huge amount of data in storage)
			// We should get at most X table Ids per level, and the chain iterator knows how to extend itself by asking
			// for more ids using GetTableIDsForRange
			chainIters := make([]iteration.Iterator, len(nonOverLapIDs))
			for j, nonOverlapID := range nonOverLapIDs {
				lazy, err := sst.NewLazySSTableIterator(nonOverlapID, s.TableCache, keyStart, keyEnd)
				if err != nil {
					return nil, err
				}
				chainIters[j] = lazy
			}
			iters[pos] = iteration.NewChainingIterator(iters)
		}
		pos++
	}

	si, err := s.newShaktiIterator(keyStart, keyEnd, iters, &s.mtLock)
	if err != nil {
		return nil, err
	}
	s.iterators[si] = struct{}{}
	return si, nil
}

func (s *Shakti) removeIterator(iter *shaktiIterator) {
	s.mtLock.Lock()
	defer s.mtLock.Unlock()
	delete(s.iterators, iter)
}

type bufSizeEstimates struct {
	mtBuffSizeEstimate uint32
	mtEntriesEstimate  uint32
}

// update estimates of buffer size and number of entries - having a good estimate improves performance as writeIter reduces or
// eliminates slice copying when original capacity is exceeded. We basically take the largest we've seen and add 5%
func (b *bufSizeEstimates) updateSizeEstimates(buffSize int, entries int) {
	// Note: It doesn't matter too much if we have a race here as writeIter's just an estimate so no need to lock we can just use an
	// atomic
	if buffSize > b.getMtBuffSizeEstimate() {
		atomic.StoreUint32(&b.mtBuffSizeEstimate, uint32(float64(buffSize)*1.05))
	}
	if entries > b.getMtEntriesEstimate() {
		atomic.StoreUint32(&b.mtEntriesEstimate, uint32(float64(entries)*1.05))
	}
}

func (b *bufSizeEstimates) getMtBuffSizeEstimate() int {
	return int(atomic.LoadUint32(&b.mtBuffSizeEstimate))
}

func (b *bufSizeEstimates) getMtEntriesEstimate() int {
	return int(atomic.LoadUint32(&b.mtEntriesEstimate))
}

type ssTableInfo struct {
	ssTableID   sst.SSTableID
	largestKey  []byte
	smallestKey []byte
}

type mtFlushEntry struct {
	memtable  *mem.Memtable
	ssTabInfo atomic.Value
}

// Called after the ssTable for the memtable has been stored to cloud storage
func (fe *mtFlushEntry) setSSTableInfo(ssTableInfo *ssTableInfo) {
	fe.ssTabInfo.Store(ssTableInfo)
	log.Debug("setting sstabinfo on entry")
}

func (fe *mtFlushEntry) getSSTableInfo() *ssTableInfo {
	s := fe.ssTabInfo.Load()
	if s == nil {
		return nil
	}
	return s.(*ssTableInfo)
}

func (s *Shakti) mtFlushRunLoop() {
	var bufEstimates bufSizeEstimates
	pos := 0
	for range s.mtFlushChan {
		s.mtFlushQueueLock.Lock()
		var i int
		// We keep memtables in the flush queue until they are actually fully stored and registered with the controller
		// and this happens asynchronously. Here we remove the flushed prefix of the flush queue
		// We make sure we register sstables in the same order they were added to the flush queue
		for i = 0; i < pos; i++ {
			fe := &s.mtFlushQueue[i]
			tabInfo := fe.getSSTableInfo()
			if tabInfo == nil {
				// Not stored in cloud storage yet
				break
			}
			log.Debugf("registering sstable %v with controller", tabInfo.ssTableID)
			if err := s.controller.ApplyChanges(datacontroller.RegistrationBatch{
				Registrations: []datacontroller.RegistrationEntry{{
					Level:    0,
					TableID:  tabInfo.ssTableID,
					KeyStart: tabInfo.smallestKey,
					KeyEnd:   tabInfo.largestKey,
				}},
				Deregistrations: nil,
			}); err != nil {
				log.Errorf("failed to register sstable %+v", err)
				return
			}
			if err := fe.memtable.Committed(); err != nil {
				log.Errorf("failed to call memtable callback %+v", err)
			}
			fe.memtable = nil
		}
		if i > 0 {
			nl := len(s.mtFlushQueue) - i
			fq := make([]mtFlushEntry, nl)
			copy(fq, s.mtFlushQueue[i:])
			s.mtFlushQueue = fq
			pos -= i
			if pos == len(s.mtFlushQueue) {
				s.mtFlushQueueLock.Unlock()
				continue
			}
		}

		log.Debugf("queue size is %d", len(s.mtFlushQueue))
		// Take next one to flush
		flushEntry := &s.mtFlushQueue[pos]
		s.mtFlushQueueLock.Unlock()
		pos++
		buffSizeEstimate := bufEstimates.getMtBuffSizeEstimate()
		entriesEstimate := bufEstimates.getMtEntriesEstimate()
		// We flush in parallel as cloud storage can have a high latency
		go func() {
			log.Debug("flushing memtable")
			buffSize, entries, err := s.flushMemtable(flushEntry, buffSizeEstimate, entriesEstimate)
			if err != nil {
				log.Errorf("failed to flush memtable %+v", err)
				return
			}
			bufEstimates.updateSizeEstimates(buffSize, entries)
		}()
	}
	s.stopWg.Done()
}

// Flush the memtable to a sstable, and push writeIter to cloud storage, this method does not register the sstable with
// the controller. Registration must be done in the same order in which memtables were created. Flushing can occur
// in parallel for multiple memtables.
func (s *Shakti) flushMemtable(flushEntry *mtFlushEntry, buffSizeEstimate int, entriesEstimate int) (int, int, error) {
	mt := flushEntry.memtable
	iter := mt.NewIterator(nil, nil)
	ssTable, smallestKey, largestKey, err := sst.BuildSSTable(s.conf.TableFormat, buffSizeEstimate, entriesEstimate,
		mt.CommonPrefix(), iter)
	if err != nil {
		return 0, 0, err
	}
	log.Debugf("flushed memtable to sstable, size %d entries %d", ssTable.SizeBytes(), ssTable.NumEntries())
	id, err := uuid.New().MarshalBinary()
	if err != nil {
		return 0, 0, err
	}
	if err := s.TableCache.AddSSTable(id, ssTable); err != nil {
		return 0, 0, err
	}
	log.Debug("added sstable to table cache")
	tableBytes := ssTable.Serialize()
	if err := s.cloudStore.Add(id, tableBytes); err != nil {
		return 0, 0, err
	}
	log.Debug("added sstable to cloud storage")
	flushEntry.setSSTableInfo(&ssTableInfo{
		ssTableID:   id,
		largestKey:  largestKey,
		smallestKey: smallestKey,
	})
	// Note we don't register the sstable with the controller here as that must be done strictly in order the sstables
	// were produced, and this function is run in parallel. The actual registration occurs on the mtRunLoop,
	// we trigger a run of the loop here
	s.mtFlushChan <- struct{}{}
	return len(tableBytes), ssTable.NumEntries(), nil
}

func (s *Shakti) scheduleMtReplace() {
	s.mtReplaceTimer = time.AfterFunc(s.conf.MemTableMaxReplaceTime, func() {
		s.startStopLock.Lock()
		defer s.startStopLock.Unlock()
		if !s.started {
			return
		}
		if err := s.maybeReplaceMemtable(); err != nil {
			log.Errorf("failed to replace memtabe %+v", err)
		}
		s.scheduleMtReplace()
	})
}

// We periodically replace the memtable if it hasn't already been replaced within a max period
func (s *Shakti) maybeReplaceMemtable() error {
	// Note we don't use time.Now() as it is *not* monotonic - it uses system time so any adjustments to system time
	// would make this go wrong
	s.mtLock.RLock()
	now := common.NanoTime()
	if s.mtLastReplace == 0 || s.mtLastReplace-now >= s.mtMaxReplaceTime {
		log.Debug("periodic replace of memtable occurring")
		mt := s.memtable
		s.mtLock.RUnlock()
		return s.replaceMemtable(mt)
	}
	s.mtLock.RUnlock()
	return nil
}

func (s *Shakti) newShaktiIterator(rangeStart []byte, rangeEnd []byte, iters []iteration.Iterator, lock *sync.RWMutex) (*shaktiIterator, error) {
	mi, err := iteration.NewMergingIterator(iters, false)
	if err != nil {
		return nil, err
	}
	si := &shaktiIterator{
		s:          s,
		lock:       lock,
		rangeStart: rangeStart,
		rangeEnd:   rangeEnd,
		mi:         mi,
	}
	return si, nil
}

type shaktiIterator struct {
	s          *Shakti
	lock       *sync.RWMutex
	rangeStart []byte
	rangeEnd   []byte
	lastKey    []byte
	mi         *iteration.MergingIterator
}

func (s *shaktiIterator) getRange() ([]byte, []byte, []byte) {
	return s.rangeStart, s.rangeEnd, s.lastKey
}

func (s *shaktiIterator) addNewMemtableIterator(iter iteration.Iterator) error {
	return s.mi.PrependIterator(iter)
}

func (s *shaktiIterator) Close() error {
	s.s.removeIterator(s)
	return nil
}

func (s *shaktiIterator) Current() cmn.KV {
	s.lock.RLock()
	defer s.lock.RUnlock()
	curr := s.mi.Current()
	s.lastKey = curr.Key
	return curr
}

func (s *shaktiIterator) Next() error {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.mi.Next()
}

func (s *shaktiIterator) IsValid() (bool, error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.mi.IsValid()
}
