package sectorstorage

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/user"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/specs-storage/storage"

	"github.com/filecoin-project/lotus/extern/sector-storage/sealtasks"
	"github.com/filecoin-project/lotus/extern/sector-storage/storiface"
)

type schedPrioCtxKey int

var SchedPriorityKey schedPrioCtxKey
var DefaultSchedPriority = 0
var SelectorTimeout = 5 * time.Second
var InitWait = 3 * time.Second

var (
	SchedWindows = 2
)


// #### yuan ####
type wokerLog struct {
	Hostname string
	Time int64
}

var workerLog = map[string]wokerLog{}
var workerLogRead = false
var workerLogLock sync.Mutex
var workerLogReadLock sync.Mutex
var workerLogFilename = ".sealing_lotus_config"
var workerLogBasePath = ""
func workerLogFilenamePath() string {
	return workerLogBasePath+ "/"+workerLogFilename
}
func wlStore(taskType sealtasks.TaskType, sectorId abi.SectorID, hostname string){
	wlRead()
	log.Infof("==== [yuan] ==== wlStore wokerlog sealtasks.TTPreCommit1 iscome ##########")
	if taskType == sealtasks.TTPreCommit1 {
		log.Infof("==== [yuan] ==== wlStore wokerlog sealtasks.TTPreCommit1 befor %+v  ##########", workerLog)
		// 添加记录
		workerLog[fmt.Sprintf("%d_%d", sectorId.Miner, sectorId.Number)] = wokerLog{
			Hostname: hostname,
			Time: time.Now().Unix(),
		}
		log.Infof("==== [yuan] ==== wlStore wokerlog sealtasks.TTPreCommit1 after %+v  ##########", workerLog)
		go func() {
			workerLogLock.Lock()
			defer  workerLogLock.Unlock()
			str, err := json.Marshal(workerLog)
			if err == nil {
				err = ioutil.WriteFile(workerLogFilenamePath(), str, 0644)
				log.Info("==== [yuan] ==== write wokerlog str:%v err:%v  ##########", err)
			} else {
				log.Warnf("==== [yuan] ==== write wokerlog to file err:%v  ##########", err)
			}
		}()
	}

}

func wlRead(){
	workerLogReadLock.Lock()
	defer workerLogReadLock.Unlock()
	if !workerLogRead {
		workerLogLock.Lock()
		defer  workerLogLock.Unlock()

		if len(workerLogBasePath) == 0 {
			u, err := user.Current()
			if nil == err {
				workerLogBasePath = u.HomeDir
			}
		}

		var file *os.File
		if _, err := os.Stat(workerLogFilenamePath()); os.IsNotExist(err) {
			file, err = os.Create(workerLogFilenamePath())
			if err != nil {
				log.Warnf("==== [yuan] ==== read wokerlog Create err:%v  ##########", err)
				return
			}

		} else {
			file, err = os.Open(workerLogFilenamePath())
			if err != nil {
				log.Warnf("==== [yuan] ==== read wokerlog fail err:%v  ##########", err)
				return
			}
		}
		defer file.Close()

		content, err := ioutil.ReadAll(file)
		if err == nil {
			err = json.Unmarshal(content, &workerLog)

			log.Infof("==== [yuan] ==== read wokerlog success workerLog:%+v  ##########", workerLog)
			nowtime := time.Now().Unix()
			for k, val := range workerLog {
				if val.Time + 86400 * 2 <= nowtime {
					delete(workerLog, k)
				}
			}
			workerLogRead = true

		} else {
			log.Warnf("==== [yuan] ==== read wokerlog ioutil.ReadAll false err:%v  ##########", err)
		}

	}

}

func wlCheck(taskType sealtasks.TaskType, sectorId abi.SectorID, hostname string) bool {
	//log.Info("==== [yuan] ==== wokerLog wlCheck isCome #####")
	wlRead()
	if taskType == sealtasks.TTPreCommit2 {
		mk := fmt.Sprintf("%d_%d", sectorId.Miner, sectorId.Number)
		log.Infof("==== [yuan] ==== wokerLog wlCheck TTPreCommit2 workerLog[sectorId]:%v #####", workerLog[mk])
		if _,ok:=workerLog[mk];ok{
			// @todo [yuan]
			if workerLog[mk].Hostname != hostname {
			//if workerLog[mk].Hostname == hostname {
				log.Info("==== [yuan] ==== wokerLog wlCheck TTPreCommit2 Hostname false #####")
				return false
			}
		}
		log.Info("==== [yuan] ==== wokerLog wlCheck TTPreCommit2 is true #####")
	}

	return true
}
// #### yuan ####


func getPriority(ctx context.Context) int {
	sp := ctx.Value(SchedPriorityKey)
	if p, ok := sp.(int); ok {
		return p
	}

	return DefaultSchedPriority
}

func WithPriority(ctx context.Context, priority int) context.Context {
	return context.WithValue(ctx, SchedPriorityKey, priority)
}

const mib = 1 << 20

type WorkerAction func(ctx context.Context, w Worker) error

type WorkerSelector interface {
	Ok(ctx context.Context, task sealtasks.TaskType, spt abi.RegisteredSealProof, a *workerHandle) (bool, error) // true if worker is acceptable for performing a task

	Cmp(ctx context.Context, task sealtasks.TaskType, a, b *workerHandle) (bool, error) // true if a is preferred over b
}

type scheduler struct {
	workersLk sync.RWMutex
	workers   map[WorkerID]*workerHandle

	schedule       chan *workerRequest
	windowRequests chan *schedWindowRequest
	workerChange   chan struct{} // worker added / changed/freed resources
	workerDisable  chan workerDisableReq

	// owned by the sh.runSched goroutine
	schedQueue  *requestQueue
	openWindows []*schedWindowRequest

	workTracker *workTracker

	info chan func(interface{})

	closing  chan struct{}
	closed   chan struct{}
	testSync chan struct{} // used for testing
}

type workerHandle struct {
	workerRpc Worker

	info storiface.WorkerInfo

	preparing *activeResources
	active    *activeResources

	lk sync.Mutex

	wndLk         sync.Mutex
	activeWindows []*schedWindow

	enabled bool

	// for sync manager goroutine closing
	cleanupStarted bool
	closedMgr      chan struct{}
	closingMgr     chan struct{}
}

type schedWindowRequest struct {
	worker WorkerID

	done chan *schedWindow
}

type schedWindow struct {
	allocated activeResources
	todo      []*workerRequest
}

type workerDisableReq struct {
	activeWindows []*schedWindow
	wid           WorkerID
	done          func()
}

type activeResources struct {
	memUsedMin uint64
	memUsedMax uint64
	gpuUsed    bool
	cpuUse     uint64

	cond *sync.Cond
}

type workerRequest struct {
	sector   storage.SectorRef
	taskType sealtasks.TaskType
	priority int // larger values more important
	sel      WorkerSelector

	prepare WorkerAction
	work    WorkerAction

	start time.Time

	index int // The index of the item in the heap.

	indexHeap int
	ret       chan<- workerResponse
	ctx       context.Context
}

type workerResponse struct {
	err error
}

func newScheduler() *scheduler {
	return &scheduler{
		workers: map[WorkerID]*workerHandle{},

		schedule:       make(chan *workerRequest),
		windowRequests: make(chan *schedWindowRequest, 20),
		workerChange:   make(chan struct{}, 20),
		workerDisable:  make(chan workerDisableReq),

		schedQueue: &requestQueue{},

		workTracker: &workTracker{
			done:    map[storiface.CallID]struct{}{},
			running: map[storiface.CallID]trackedWork{},
		},

		info: make(chan func(interface{})),

		closing: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (sh *scheduler) Schedule(ctx context.Context, sector storage.SectorRef, taskType sealtasks.TaskType, sel WorkerSelector, prepare WorkerAction, work WorkerAction) error {
	ret := make(chan workerResponse)

	select {
	case sh.schedule <- &workerRequest{
		sector:   sector,
		taskType: taskType,
		priority: getPriority(ctx),
		sel:      sel,

		prepare: prepare,
		work:    work,

		start: time.Now(),

		ret: ret,
		ctx: ctx,
	}:
	case <-sh.closing:
		return xerrors.New("closing")
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case resp := <-ret:
		return resp.err
	case <-sh.closing:
		return xerrors.New("closing")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *workerRequest) respond(err error) {
	select {
	case r.ret <- workerResponse{err: err}:
	case <-r.ctx.Done():
		log.Warnf("request got cancelled before we could respond")
	}
}

type SchedDiagRequestInfo struct {
	Sector   abi.SectorID
	TaskType sealtasks.TaskType
	Priority int
}

type SchedDiagInfo struct {
	Requests    []SchedDiagRequestInfo
	OpenWindows []string
}

func (sh *scheduler) runSched() {
	defer close(sh.closed)

	iw := time.After(InitWait)
	var initialised bool

	// @todo [yuan] read workerLog
	wlRead()
	log.Info("==== [yuan] [runSched] workerLog wlRead #######")
	log.Info("==== [yuan] [runSched] workerLog wlRead #######")
	log.Info("==== [yuan] [runSched] workerLog wlRead #######")
	log.Info("==== [yuan] [runSched] workerLog wlRead #######")
	log.Info("==== [yuan] [runSched] workerLog wlRead #######")
	log.Info("==== [yuan] [runSched] workerLog wlRead #######")

	for {
		var doSched bool
		var toDisable []workerDisableReq

		select {
		case <-sh.workerChange:
			// @todo yuan
			log.Infof("==== [yuan] [runSched] ==== sh.workerChange ")
			doSched = true
		case dreq := <-sh.workerDisable:
			toDisable = append(toDisable, dreq)
			doSched = true
		case req := <-sh.schedule:
			// @todo yuan
			log.Infof("==== [yuan] [runSched] ==== sh.schedule req")
			log.Infof("==== [yuan] [runSched] ==== sh.schedule req:%+v", req)
			sh.schedQueue.Push(req)
			doSched = true

			if sh.testSync != nil {
				sh.testSync <- struct{}{}
			}
		case req := <-sh.windowRequests:
			log.Infof("==== [yuan] [runSched] ==== sh.windowRequests req log")
			log.Infof("==== [yuan] [runSched] ==== sh.windowRequests req wokerStr:%+v  ", req.worker.String())
			sh.openWindows = append(sh.openWindows, req)
			doSched = true
		case ireq := <-sh.info:
			ireq(sh.diag())

		case <-iw:
			initialised = true
			iw = nil
			doSched = true
		case <-sh.closing:
			sh.schedClose()
			return
		}

		if doSched && initialised {
			// First gather any pending tasks, so we go through the scheduling loop
			// once for every added task
		loop:
			for {
				select {
				case <-sh.workerChange:
				case dreq := <-sh.workerDisable:
					toDisable = append(toDisable, dreq)
				case req := <-sh.schedule:
					sh.schedQueue.Push(req)
					if sh.testSync != nil {
						sh.testSync <- struct{}{}
					}
				case req := <-sh.windowRequests:
					sh.openWindows = append(sh.openWindows, req)
				default:
					break loop
				}
			}

			for _, req := range toDisable {
				for _, window := range req.activeWindows {
					for _, request := range window.todo {
						sh.schedQueue.Push(request)
					}
				}

				openWindows := make([]*schedWindowRequest, 0, len(sh.openWindows))
				for _, window := range sh.openWindows {
					if window.worker != req.wid {
						openWindows = append(openWindows, window)
					}
				}
				sh.openWindows = openWindows

				sh.workersLk.Lock()
				sh.workers[req.wid].enabled = false
				sh.workersLk.Unlock()

				req.done()
			}

			sh.trySched()
		}

	}
}

func (sh *scheduler) diag() SchedDiagInfo {
	var out SchedDiagInfo

	for sqi := 0; sqi < sh.schedQueue.Len(); sqi++ {
		task := (*sh.schedQueue)[sqi]

		out.Requests = append(out.Requests, SchedDiagRequestInfo{
			Sector:   task.sector.ID,
			TaskType: task.taskType,
			Priority: task.priority,
		})
	}

	sh.workersLk.RLock()
	defer sh.workersLk.RUnlock()

	for _, window := range sh.openWindows {
		out.OpenWindows = append(out.OpenWindows, uuid.UUID(window.worker).String())
	}

	return out
}

func (sh *scheduler) trySched() {
	/*
		This assigns tasks to workers based on:
		- Task priority (achieved by handling sh.schedQueue in order, since it's already sorted by priority)
		- Worker resource availability
		- Task-specified worker preference (acceptableWindows array below sorted by this preference)
		- Window request age

		1. For each task in the schedQueue find windows which can handle them
		1.1. Create list of windows capable of handling a task
		1.2. Sort windows according to task selector preferences
		2. Going through schedQueue again, assign task to first acceptable window
		   with resources available
		3. Submit windows with scheduled tasks to workers

	*/

	sh.workersLk.RLock()
	defer sh.workersLk.RUnlock()

	windowsLen := len(sh.openWindows)
	queueLen := sh.schedQueue.Len()

	log.Debugf("SCHED %d queued; %d open windows", queueLen, windowsLen)
	//// @todo yuan
	//log.Infof("==== [yuan] ==== [SCHED] ")
	//log.Infof("==== [yuan] ==== [SCHED] sh.openWindows:%+v", sh.openWindows)
	//log.Infof("==== [yuan] ==== [SCHED] sh.schedQueue:%+v", sh.schedQueue)
	//log.Infof("==== [yuan] ==== [SCHED] ")
	if windowsLen == 0 || queueLen == 0 {
		// nothing to schedule on
		return
	}

	windows := make([]schedWindow, windowsLen)
	acceptableWindows := make([][]int, queueLen)

	// Step 1
	throttle := make(chan struct{}, windowsLen)

	var wg sync.WaitGroup
	wg.Add(queueLen)
	for i := 0; i < queueLen; i++ {
		throttle <- struct{}{}

		go func(sqi int) {
			defer wg.Done()
			defer func() {
				<-throttle
			}()

			task := (*sh.schedQueue)[sqi]
			needRes := ResourceTable[task.taskType][task.sector.ProofType]
			if task.taskType == sealtasks.TTPreCommit1 || task.taskType == sealtasks.TTPreCommit2 {
				// @todo yuan
				log.Infof("==== [yuan] ==== [SCHED] [task]")
				//log.Infof("==== [yuan] ==== [SCHED] [task] task:%+v ", task)
			}

			task.indexHeap = sqi
			for wnd, windowRequest := range sh.openWindows {
				worker, ok := sh.workers[windowRequest.worker]
				if !ok {
					log.Errorf("worker referenced by windowRequest not found (worker: %s)", windowRequest.worker)
					// TODO: How to move forward here?
					continue
				}

				// @todo yuan
				if task.taskType == sealtasks.TTPreCommit1 || task.taskType == sealtasks.TTPreCommit2 {
					log.Infof("==== [yuan] ==== [SCHED] [windowRequest] windowRequest:%+v", windowRequest.worker.String())
					log.Infof("==== [yuan] ==== [SCHED] [task] [for] wlCheck:%v   worker.info:%+v", wlCheck(task.taskType, task.sector.ID, worker.info.Hostname), worker.info)

				}

				// @todo [yuan] check hostname is same machine
				if !wlCheck(task.taskType, task.sector.ID, worker.info.Hostname) {
					continue
				}

				if !worker.enabled {
					log.Debugw("skipping disabled worker", "worker", windowRequest.worker)
					continue
				}

				// TODO: allow bigger windows
				if !windows[wnd].allocated.canHandleRequest(needRes, windowRequest.worker, "schedAcceptable", worker.info) {
					continue
				}

				// @todo [yuan] check hostname is same machine
				if !wlCheck(task.taskType, task.sector.ID, worker.info.Hostname) {
					continue
				}

				rpcCtx, cancel := context.WithTimeout(task.ctx, SelectorTimeout)
				ok, err := task.sel.Ok(rpcCtx, task.taskType, task.sector.ProofType, worker)
				cancel()
				if err != nil {
					log.Errorf("trySched(1) req.sel.Ok error: %+v", err)
					continue
				}

				if !ok {
					continue
				}

				acceptableWindows[sqi] = append(acceptableWindows[sqi], wnd)
			}

			if len(acceptableWindows[sqi]) == 0 {
				return
			}

			// Pick best worker (shuffle in case some workers are equally as good)
			rand.Shuffle(len(acceptableWindows[sqi]), func(i, j int) {
				acceptableWindows[sqi][i], acceptableWindows[sqi][j] = acceptableWindows[sqi][j], acceptableWindows[sqi][i] // nolint:scopelint
			})
			sort.SliceStable(acceptableWindows[sqi], func(i, j int) bool {
				wii := sh.openWindows[acceptableWindows[sqi][i]].worker // nolint:scopelint
				wji := sh.openWindows[acceptableWindows[sqi][j]].worker // nolint:scopelint

				if wii == wji {
					// for the same worker prefer older windows
					return acceptableWindows[sqi][i] < acceptableWindows[sqi][j] // nolint:scopelint
				}

				wi := sh.workers[wii]
				wj := sh.workers[wji]

				rpcCtx, cancel := context.WithTimeout(task.ctx, SelectorTimeout)
				defer cancel()

				r, err := task.sel.Cmp(rpcCtx, task.taskType, wi, wj)
				if err != nil {
					log.Errorf("selecting best worker: %s", err)
				}
				return r
			})
		}(i)
	}

	wg.Wait()

	log.Debugf("SCHED windows: %+v", windows)
	log.Debugf("SCHED Acceptable win: %+v", acceptableWindows)

	// Step 2
	scheduled := 0
	rmQueue := make([]int, 0, queueLen)

	for sqi := 0; sqi < queueLen; sqi++ {
		task := (*sh.schedQueue)[sqi]
		needRes := ResourceTable[task.taskType][task.sector.ProofType]

		selectedWindow := -1
		for _, wnd := range acceptableWindows[task.indexHeap] {
			wid := sh.openWindows[wnd].worker
			info := sh.workers[wid].info

			log.Debugf("SCHED try assign sqi:%d sector %d to window %d", sqi, task.sector.ID.Number, wnd)
			log.Debugf("SCHED try assign wokers_info:%+v", info)

			// TODO: allow bigger windows
			if !windows[wnd].allocated.canHandleRequest(needRes, wid, "schedAssign", info) {
				continue
			}

			log.Debugf("SCHED ASSIGNED sqi:%d sector %d task %s to window %d", sqi, task.sector.ID.Number, task.taskType, wnd)

			windows[wnd].allocated.add(info.Resources, needRes)
			// TODO: We probably want to re-sort acceptableWindows here based on new
			//  workerHandle.utilization + windows[wnd].allocated.utilization (workerHandle.utilization is used in all
			//  task selectors, but not in the same way, so need to figure out how to do that in a non-O(n^2 way), and
			//  without additional network roundtrips (O(n^2) could be avoided by turning acceptableWindows.[] into heaps))

			selectedWindow = wnd
			break
		}

		if selectedWindow < 0 {
			// all windows full
			continue
		}

		windows[selectedWindow].todo = append(windows[selectedWindow].todo, task)

		rmQueue = append(rmQueue, sqi)
		scheduled++
	}

	if len(rmQueue) > 0 {
		for i := len(rmQueue) - 1; i >= 0; i-- {
			sh.schedQueue.Remove(rmQueue[i])
		}
	}

	// Step 3

	if scheduled == 0 {
		return
	}

	scheduledWindows := map[int]struct{}{}
	for wnd, window := range windows {
		if len(window.todo) == 0 {
			// Nothing scheduled here, keep the window open
			continue
		}

		scheduledWindows[wnd] = struct{}{}

		window := window // copy
		select {
		case sh.openWindows[wnd].done <- &window:
		default:
			log.Error("expected sh.openWindows[wnd].done to be buffered")
		}
	}

	// Rewrite sh.openWindows array, removing scheduled windows
	newOpenWindows := make([]*schedWindowRequest, 0, windowsLen-len(scheduledWindows))
	for wnd, window := range sh.openWindows {
		if _, scheduled := scheduledWindows[wnd]; scheduled {
			// keep unscheduled windows open
			continue
		}

		newOpenWindows = append(newOpenWindows, window)
	}

	sh.openWindows = newOpenWindows
}

func (sh *scheduler) schedClose() {
	sh.workersLk.Lock()
	defer sh.workersLk.Unlock()
	log.Debugf("closing scheduler")

	for i, w := range sh.workers {
		sh.workerCleanup(i, w)
	}
}

func (sh *scheduler) Info(ctx context.Context) (interface{}, error) {
	ch := make(chan interface{}, 1)

	sh.info <- func(res interface{}) {
		ch <- res
	}

	select {
	case res := <-ch:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (sh *scheduler) Close(ctx context.Context) error {
	close(sh.closing)
	select {
	case <-sh.closed:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
