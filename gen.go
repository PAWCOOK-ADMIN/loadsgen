package loadgen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"loadgen/lib"
	"loadgen/log"
)

// 日志记录器。
var logger = log.DLogger()

// myGenerator 代表载荷发生器的实现类型。
type myGenerator struct {
	caller      lib.Caller           // 调用器。
	timeoutNS   time.Duration        // 处理超时时间，单位：纳秒。
	lps         uint32               // 每秒载荷量。
	durationNS  time.Duration        // 负载持续时间，单位：纳秒。
	concurrency uint32               // 载荷并发量。
	tickets     lib.GoTickets        // Goroutine票池。
	ctx         context.Context      // 上下文。作为传递通知的载体
	cancelFunc  context.CancelFunc   // 取消函数。可用于手动停止
	callCount   int64                // 调用计数。
	status      uint32               // 载荷发生器的状态。
	resultCh    chan *lib.CallResult // 调用结果通道。
}

// NewGenerator 会新建一个载荷发生器。
func NewGenerator(pset ParamSet) (lib.Generator, error) {

	logger.Infoln("New a load generator...")
	if err := pset.Check(); err != nil {
		return nil, err
	}
	gen := &myGenerator{
		caller:     pset.Caller,
		timeoutNS:  pset.TimeoutNS,
		lps:        pset.LPS,
		durationNS: pset.DurationNS,
		status:     lib.STATUS_ORIGINAL,
		resultCh:   pset.ResultCh,
	}
	if err := gen.init(); err != nil {
		return nil, err
	}
	return gen, nil
}

// 初始化载荷发生器。
func (gen *myGenerator) init() error {
	var buf bytes.Buffer
	buf.WriteString("Initializing the load generator...")
	// 载荷的并发量 ≈ 载荷的响应超时时间 / 载荷的发送间隔时间
	var total64 = int64(gen.timeoutNS)/int64(1e9/gen.lps) + 1
	if total64 > math.MaxInt32 {
		total64 = math.MaxInt32
	}
	gen.concurrency = uint32(total64)
	tickets, err := lib.NewGoTickets(gen.concurrency)
	if err != nil {
		return err
	}
	gen.tickets = tickets

	buf.WriteString(fmt.Sprintf("Done. (concurrency=%d)", gen.concurrency))
	logger.Infoln(buf.String())
	return nil
}

// callOne 会向载荷承受方发起一次调用。
func (gen *myGenerator) callOne(rawReq *lib.RawReq) *lib.RawResp {
	atomic.AddInt64(&gen.callCount, 1) // 调用计数加1
	if rawReq == nil {
		return &lib.RawResp{ID: -1, Err: errors.New("Invalid raw request.")}
	}

	start := time.Now().UnixNano() // 起始时间，获取纳秒级别的时间戳
	resp, err := gen.caller.Call(rawReq.Req, gen.timeoutNS)
	end := time.Now().UnixNano() // 结束时间

	elapsedTime := time.Duration(end - start) // 耗时

	var rawResp lib.RawResp
	if err != nil {
		errMsg := fmt.Sprintf("Sync Call Error: %s.", err)
		rawResp = lib.RawResp{
			ID:     rawReq.ID,
			Err:    errors.New(errMsg),
			Elapse: elapsedTime}
	} else {
		rawResp = lib.RawResp{
			ID:     rawReq.ID,
			Resp:   resp,
			Elapse: elapsedTime}
	}
	return &rawResp
}

// asyncSend 会异步地调用承受方接口。每次调用都会有一个专用G被启用
func (gen *myGenerator) asyncCall() {
	gen.tickets.Take()
	go func() {
		defer func() {
			if p := recover(); p != nil {
				err, ok := interface{}(p).(error)
				var errMsg string
				if ok {
					errMsg = fmt.Sprintf("Async Call Panic! (error: %s)", err)
				} else {
					errMsg = fmt.Sprintf("Async Call Panic! (clue: %#v)", p)
				}
				logger.Errorln(errMsg)
				result := &lib.CallResult{
					ID:   -1,
					Code: lib.RET_CODE_FATAL_CALL,
					Msg:  errMsg}
				gen.sendResult(result)
			}
			gen.tickets.Return()
		}()

		rawReq := gen.caller.BuildReq() // 生成一个载荷

		// 调用状态：0-未调用或调用中；1-调用完成；2-调用超时。
		var callStatus uint32
		timer := time.AfterFunc(gen.timeoutNS, func() { // 设置超时处理
			if !atomic.CompareAndSwapUint32(&callStatus, 0, 2) { // 如何交换未成功，则说明载荷响应操作已完成，忽略超时处理
				return
			}
			result := &lib.CallResult{
				ID:     rawReq.ID,
				Req:    rawReq,
				Code:   lib.RET_CODE_WARNING_CALL_TIMEOUT,
				Msg:    fmt.Sprintf("Timeout! (expected: < %v)", gen.timeoutNS),
				Elapse: gen.timeoutNS, // 设置为最大超时时间
			}
			gen.sendResult(result)
		})

		rawResp := gen.callOne(&rawReq) // 发送载荷至接口
		if !atomic.CompareAndSwapUint32(&callStatus, 0, 1) {
			return
		}
		timer.Stop() // 响应处理前要先停掉前面启动的定时器

		var result *lib.CallResult
		if rawResp.Err != nil {
			result = &lib.CallResult{
				ID:     rawResp.ID,
				Req:    rawReq,
				Code:   lib.RET_CODE_ERROR_CALL,
				Msg:    rawResp.Err.Error(),
				Elapse: rawResp.Elapse}
		} else {
			result = gen.caller.CheckResp(rawReq, *rawResp)
			result.Elapse = rawResp.Elapse
		}
		gen.sendResult(result)
	}()
}

// sendResult 用于发送调用结果。
func (gen *myGenerator) sendResult(result *lib.CallResult) bool {
	if atomic.LoadUint32(&gen.status) != lib.STATUS_STARTED {
		gen.printIgnoredResult(result, "stopped load generator")
		return false
	}

	select {
	case gen.resultCh <- result:
		return true
	default:
		gen.printIgnoredResult(result, "full result channel")
		return false
	}
}

// printIgnoredResult 打印被忽略的结果。
func (gen *myGenerator) printIgnoredResult(result *lib.CallResult, cause string) {
	resultMsg := fmt.Sprintf(
		"ID=%d, Code=%d, Msg=%s, Elapse=%v",
		result.ID, result.Code, result.Msg, result.Elapse)
	logger.Warnf("Ignored result: %s. (cause: %s)\n", resultMsg, cause)
}

// prepareStop 用于为停止载荷发生器做准备。
func (gen *myGenerator) prepareToStop(ctxError error) {
	logger.Infof("Prepare to stop load generator (cause: %s)...", ctxError)
	atomic.CompareAndSwapUint32(
		&gen.status, lib.STATUS_STARTED, lib.STATUS_STOPPING)
	logger.Infof("Closing result channel...")
	close(gen.resultCh)
	atomic.StoreUint32(&gen.status, lib.STATUS_STOPPED)
}

// genLoad 会产生载荷并向承受方发送。
func (gen *myGenerator) genLoad(throttle <-chan time.Time) {
	for {
		select {
		case <-gen.ctx.Done(): // 多个满足条件的case之间做伪随机选择时的不确定性，以使载荷发生器总能及时地停止
			gen.prepareToStop(gen.ctx.Err())
			return
		default:
		}

		gen.asyncCall() // 发送载荷

		if gen.lps > 0 { // 表示节流阀是有效并需要使用的
			select {
			case <-throttle:
			// 周期性定时器
			case <-gen.ctx.Done(): // 该通道会在上下文超时或者取消时关闭
				gen.prepareToStop(gen.ctx.Err())
				return
			}
		}
	}
}

// Start 会启动载荷发生器。
func (gen *myGenerator) Start() bool {
	logger.Infoln("Starting load generator...")
	// 检查是否具备可启动的状态，顺便设置状态为正在启动
	if !atomic.CompareAndSwapUint32(
		&gen.status, lib.STATUS_ORIGINAL, lib.STATUS_STARTING) {
		if !atomic.CompareAndSwapUint32(
			&gen.status, lib.STATUS_STOPPED, lib.STATUS_STARTING) {
			return false
		}
	}

	// 设定节流阀。
	var throttle <-chan time.Time
	if gen.lps > 0 {
		interval := time.Duration(1e9 / gen.lps) // 即发送一个载荷需要的时间
		logger.Infof("Setting throttle (%v)...", interval)
		throttle = time.Tick(interval) // 单位ns。创建了一个周期性的定时器，并返回其通道C
	}

	// 初始化上下文和取消函数。
	gen.ctx, gen.cancelFunc = context.WithTimeout(
		context.Background(), gen.durationNS)

	// 初始化调用计数。
	gen.callCount = 0

	// 设置状态为已启动。
	atomic.StoreUint32(&gen.status, lib.STATUS_STARTED) // 原子操作，原子性地将一个无符号 32 位整数（uint32）存储到指定内存地址的函数

	go func() { // 新建一个协程启动载荷发生器
		// 生成并发送载荷。
		logger.Infoln("Generating loads...")
		gen.genLoad(throttle)
		logger.Infof("Stopped. (call count: %d)", gen.callCount) // 打印调用的总次数
	}()

	return true // 返回
}

// 手动停止
func (gen *myGenerator) Stop() bool {
	if !atomic.CompareAndSwapUint32(
		&gen.status, lib.STATUS_STARTED, lib.STATUS_STOPPING) {
		return false
	}

	gen.cancelFunc() // 执行此方法可以让ctx字段发出停止“信号”

	for { // 不断检查状态的变更
		if atomic.LoadUint32(&gen.status) == lib.STATUS_STOPPED {
			break
		}
		time.Sleep(time.Microsecond)
	}
	return true
}

func (gen *myGenerator) Status() uint32 {
	return atomic.LoadUint32(&gen.status)
}

func (gen *myGenerator) CallCount() int64 {
	return atomic.LoadInt64(&gen.callCount)
}
