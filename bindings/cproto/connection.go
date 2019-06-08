package cproto

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/restream/reindexer/bindings"
	"github.com/restream/reindexer/cjson"
)

type bufPtr struct {
	rseq uint32
	buf  *NetBuffer
}

type sig chan bufPtr

const bufsCap = 16 * 1024
const queueSize = 40
const maxSeqNum = queueSize * 10000000

const cprotoMagic = 0xEEDD1132
const cprotoVersion = 0x102
const cprotoMinCompatVersion = 0x101
const cprotoHdrLen = 16
const deadlineCheckPeriodSec = 1

const (
	cmdPing             = 0
	cmdLogin            = 1
	cmdOpenDatabase     = 2
	cmdCloseDatabase    = 3
	cmdDropDatabase     = 4
	cmdOpenNamespace    = 16
	cmdCloseNamespace   = 17
	cmdDropNamespace    = 18
	cmdAddIndex         = 21
	cmdEnumNamespaces   = 22
	cmdDropIndex        = 24
	cmdUpdateIndex      = 25
	cmdAddTxItem        = 26
	cmdCommitTx         = 27
	cmdRollbackTx       = 28
	cmdStartTransaction = 29
	cmdCommit           = 32
	cmdModifyItem       = 33
	cmdDeleteQuery      = 34
	cmdUpdateQuery      = 35
	cmdSelect           = 48
	cmdSelectSQL        = 49
	cmdFetchResults     = 50
	cmdCloseResults     = 51
	cmdGetMeta          = 64
	cmdPutMeta          = 65
	cmdEnumMeta         = 66
	cmdCodeMax          = 128
)

type requestInfo struct {
	seqNum    uint32
	repl      sig
	deadline  uint32
	timeoutCh chan uint32
}

type connection struct {
	owner *NetCProto
	conn  net.Conn

	wrBuf, wrBuf2 *bytes.Buffer
	wrKick        chan struct{}

	rdBuf *bufio.Reader

	seqs chan uint32
	lock sync.RWMutex

	err   error
	errCh chan struct{}

	lastReadStamp int64

	now    uint32
	termCh chan struct{}

	requests [queueSize]requestInfo
}

func newConnection(owner *NetCProto) (c *connection, err error) {
	c = &connection{
		owner:  owner,
		wrBuf:  bytes.NewBuffer(make([]byte, 0, bufsCap)),
		wrBuf2: bytes.NewBuffer(make([]byte, 0, bufsCap)),
		wrKick: make(chan struct{}, 1),
		seqs:   make(chan uint32, queueSize),
		errCh:  make(chan struct{}),
		termCh: make(chan struct{}),
	}
	for i := 0; i < queueSize; i++ {
		c.seqs <- uint32(i)
		c.requests[i].repl = make(sig)
		c.requests[i].timeoutCh = make(chan uint32)
	}

	go c.deadlineTicker()

	loginTimeout := uint32(owner.timeouts.LoginTimeout / time.Second)
	if err = c.connect(loginTimeout); err != nil {
		c.onError(err)
		return
	}

	if loginTimeout != 0 {
		if loginTimeout > atomic.LoadUint32(&c.now) {
			loginTimeout = loginTimeout - atomic.LoadUint32(&c.now)
		} else {
			c.onError(bindings.NewError("Connect timeout", bindings.ErrTimeout))
			return
		}
	}

	if err = c.login(owner, loginTimeout); err != nil {
		c.onError(err)
		return
	}
	return
}

func seqNumIsValid(seqNum uint32) bool {
	if seqNum < maxSeqNum {
		return true
	}
	return false
}

func (c *connection) deadlineTicker() {
	timeout := time.Second * time.Duration(deadlineCheckPeriodSec)
	ticker := time.NewTicker(timeout)
	atomic.StoreUint32(&c.now, 0)
	for range ticker.C {
		select {
		case <-c.errCh:
			return
		case <-c.termCh:
			return
		default:
		}
		now := atomic.AddUint32(&c.now, deadlineCheckPeriodSec)
		for i := range c.requests {
			seqNum := atomic.LoadUint32(&c.requests[i].seqNum)
			if !seqNumIsValid(seqNum) {
				continue
			}
			deadline := atomic.LoadUint32(&c.requests[i].deadline)
			if deadline != 0 && now >= deadline {
				atomic.StoreUint32(&c.requests[i].deadline, 0)
				timeoutCh := c.requests[i].timeoutCh
				timeoutCh <- seqNum
			}
		}
	}
}

func (c *connection) connect(timeout uint32) (err error) {
	if timeout == 0 {
		c.conn, err = net.Dial("tcp", c.owner.url.Host)
	} else {
		c.conn, err = net.DialTimeout("tcp", c.owner.url.Host, time.Duration(timeout)*time.Second)
	}
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return bindings.NewError("Connect timeout", bindings.ErrTimeout)
		}
		return err
	}
	c.conn.(*net.TCPConn).SetNoDelay(true)
	c.rdBuf = bufio.NewReaderSize(c.conn, bufsCap)

	go c.writeLoop()
	go c.readLoop()
	return
}

func (c *connection) login(owner *NetCProto, timeout uint32) (err error) {
	password, username, path := "", "", owner.url.Path
	if owner.url.User != nil {
		username = owner.url.User.Username()
		password, _ = owner.url.User.Password()
	}
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}

	buf, err := c.rpcCall(context.TODO(), cmdLogin, timeout, username, password, path)
	if err != nil {
		if rdxError, ok := err.(bindings.Error); ok && rdxError.Code() == bindings.ErrTimeout {
			c.err = bindings.NewError("Login timeout", bindings.ErrTimeout)
		} else {
			c.err = err
		}
		return
	}
	defer buf.Free()

	if len(buf.args) > 1 {
		owner.checkServerStartTime(buf.args[1].(int64))
	}
	return
}

func (c *connection) readLoop() {
	var err error
	var hdr = make([]byte, cprotoHdrLen)
	for {
		if err = c.readReply(hdr); err != nil {
			c.onError(err)
			return
		}
		atomic.StoreInt64(&c.lastReadStamp, time.Now().Unix())
	}
}

func (c *connection) readReply(hdr []byte) (err error) {
	if _, err = io.ReadFull(c.rdBuf, hdr); err != nil {
		return
	}
	ser := cjson.NewSerializer(hdr)
	magic := ser.GetUInt32()

	if magic != cprotoMagic {
		return fmt.Errorf("Invalid cproto magic '%08X'", magic)
	}

	version := ser.GetUInt16()
	_ = int(ser.GetUInt16())
	size := int(ser.GetUInt32())
	rseq := uint32(ser.GetUInt32())

	if version < cprotoMinCompatVersion {
		return fmt.Errorf("Unsupported cproto version '%04X'. This client expects reindexer server v1.9.8+", version)
	}

	if !seqNumIsValid(rseq) {
		return fmt.Errorf("invalid seq num: %d", rseq)
	}
	reqID := rseq % queueSize
	if atomic.LoadUint32(&c.requests[reqID].seqNum) != rseq {
		io.CopyN(ioutil.Discard, c.rdBuf, int64(size))
		return
	}
	repCh := c.requests[reqID].repl
	answ := newNetBuffer()
	answ.reset(size, c)

	if _, err = io.ReadFull(c.rdBuf, answ.buf); err != nil {
		return
	}

	if repCh != nil {
		repCh <- bufPtr{rseq, answ}
	} else {
		return fmt.Errorf("unexpected answer: %v", answ)
	}
	return
}

func (c *connection) write(buf []byte) {
	c.lock.Lock()
	c.wrBuf.Write(buf)
	c.lock.Unlock()
	select {
	case c.wrKick <- struct{}{}:
	default:
	}
}

func (c *connection) writeLoop() {
	for {
		select {
		case <-c.errCh:
			return
		case <-c.wrKick:
		}
		c.lock.Lock()
		if c.wrBuf.Len() == 0 {
			err := c.err
			c.lock.Unlock()
			if err == nil {
				continue
			} else {
				return
			}
		}
		c.wrBuf, c.wrBuf2 = c.wrBuf2, c.wrBuf
		c.lock.Unlock()

		if _, err := c.wrBuf2.WriteTo(c.conn); err != nil {
			c.onError(err)
			return
		}
	}
}

func nextSeqNum(seqNum uint32) uint32 {
	seqNum += queueSize
	if seqNum < maxSeqNum {
		return seqNum
	}
	return seqNum - maxSeqNum
}

func (c *connection) rpcCall(ctx context.Context, cmd int, netTimeout uint32, args ...interface{}) (buf *NetBuffer, err error) {
	seq := <-c.seqs
	reqID := seq % queueSize
	reply := c.requests[reqID].repl
	timeoutCh := c.requests[reqID].timeoutCh

	var execTimeout int
	if execDeadline, ok := ctx.Deadline(); ok {
		execTimeout = int(execDeadline.Sub(time.Now()) / time.Millisecond)
		if execTimeout <= 0 {
			return nil, bindings.NewError("Request was canceled", bindings.ErrCanceled)
		}
	}

	if netTimeout != 0 {
		atomic.StoreUint32(&c.requests[reqID].deadline, atomic.LoadUint32(&c.now)+netTimeout)
	}
	atomic.StoreUint32(&c.requests[reqID].seqNum, seq)
	in := newRPCEncoder(cmd, seq)
	for _, a := range args {
		switch t := a.(type) {
		case bool:
			in.boolArg(t)
		case int:
			in.intArg(t)
		case int32:
			in.intArg(int(t))
		case int64:
			in.int64Arg(t)
		case string:
			in.stringArg(t)
		case []byte:
			in.bytesArg(t)
		case []int32:
			in.int32ArrArg(t)
		}
	}

	in.startArgsChunck()
	in.int64Arg(int64(execTimeout))

	c.write(in.ser.Bytes())
	in.ser.Close()

for_loop:
	for {
		select {
		case bufPtr := <-reply:
			if bufPtr.rseq == seq {
				buf = bufPtr.buf
				break for_loop
			}
		case <-c.errCh:
			c.lock.RLock()
			err = c.err
			c.lock.RUnlock()
			break for_loop
		case timeoutSeq := <-timeoutCh:
			if timeoutSeq == seq {
				err = bindings.NewError("Request timeout", bindings.ErrTimeout)
				break for_loop
			}
		}
	}
	atomic.StoreUint32(&c.requests[reqID].seqNum, maxSeqNum)

	c.seqs <- nextSeqNum(seq)
	if err != nil {
		return
	}
	if err = buf.parseArgs(); err != nil {
		return
	}
	return
}

func (c *connection) onError(err error) {
	c.lock.Lock()
	if c.err == nil {
		c.err = err
		if c.conn != nil {
			c.conn.Close()
		}
		select {
		case <-c.errCh:
		default:
			close(c.errCh)
		}
	}
	c.lock.Unlock()
}

func (c *connection) hasError() (has bool) {
	c.lock.RLock()
	has = c.err != nil
	c.lock.RUnlock()
	return
}

func (c *connection) lastReadTime() time.Time {
	stamp := atomic.LoadInt64(&c.lastReadStamp)
	return time.Unix(stamp, 0)
}

func (c *connection) Finalize() error {
	close(c.termCh)
	return nil
}
