package utils

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/banbox/banbot/btime"
	"github.com/banbox/banbot/core"
	"github.com/banbox/banexg/errs"
	"github.com/banbox/banexg/log"
	"github.com/banbox/banexg/utils"
	"github.com/bytedance/sonic"
	"github.com/mitchellh/mapstructure"
	"go.uber.org/zap"
	"io"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ConnCB = func(string, interface{})

type IBanConn interface {
	WriteMsg(msg *IOMsg) *errs.Error
	Write(data []byte) *errs.Error
	ReadMsg() (*IOMsg, *errs.Error)
	Subscribe(tags ...string)
	UnSubscribe(tags ...string)
	GetRemote() string
	IsClosed() bool
	HasTag(tag string) bool
	RunForever() *errs.Error
}

type BanConn struct {
	Conn       net.Conn          // 原始的socket连接
	Tags       map[string]bool   // 消息订阅列表
	Remote     string            // 远端名称
	Listens    map[string]ConnCB // 消息处理函数
	RefreshMS  int64             // 连接就绪的时间戳
	Ready      bool
	m          sync.Mutex
	DoConnect  func(conn *BanConn) // 重新连接函数，未提供不尝试重新连接
	ReInitConn func()              // 重新连接成功后初始化回调函数
}

type IOMsg struct {
	Action string
	Data   interface{}
}

func (c *BanConn) GetRemote() string {
	return c.Remote
}
func (c *BanConn) IsClosed() bool {
	return c.Conn == nil || !c.Ready
}
func (c *BanConn) HasTag(tag string) bool {
	_, ok := c.Tags[tag]
	return ok
}

func (c *BanConn) WriteMsg(msg *IOMsg) *errs.Error {
	raw, err_ := sonic.Marshal(*msg)
	if err_ != nil {
		return errs.New(core.ErrMarshalFail, err_)
	}
	compressed, err := compress(raw)
	if err != nil {
		return err
	}
	c.m.Lock()
	defer c.m.Unlock()
	return c.Write(compressed)
}

func (c *BanConn) Write(data []byte) *errs.Error {
	dataLen := uint32(len(data))
	lenBt := make([]byte, 4)
	binary.LittleEndian.PutUint32(lenBt, dataLen)
	_, err_ := c.Conn.Write(lenBt)
	if err_ != nil {
		c.Ready = false
		errCode, errType := getErrType(err_)
		if c.DoConnect != nil && errCode == core.ErrNetConnect {
			log.Warn("write fail, wait 3s and retry", zap.String("type", errType))
			c.connect(false)
			return c.Write(data)
		}
		return errs.New(errCode, err_)
	}
	_, err_ = c.Conn.Write(data)
	if err_ != nil {
		c.Ready = false
		return errs.New(core.ErrNetWriteFail, err_)
	}
	return nil
}

func (c *BanConn) ReadMsg() (*IOMsg, *errs.Error) {
	compressed, err := c.Read()
	if err != nil {
		return nil, err
	}
	data, err := deCompress(compressed)
	if err != nil {
		return nil, err
	}
	var msg IOMsg
	err_ := utils.Unmarshal(data, &msg)
	if err_ != nil {
		return nil, errs.New(errs.CodeUnmarshalFail, err_)
	}
	return &msg, nil
}

func (c *BanConn) Read() ([]byte, *errs.Error) {
	lenBuf := make([]byte, 4)
	_, err_ := c.Conn.Read(lenBuf)
	if err_ != nil {
		errCode, errType := getErrType(err_)
		if c.DoConnect != nil && errCode == core.ErrNetConnect {
			log.Warn("read fail, wait 3s and retry", zap.String("type", errType))
			c.connect(true)
			return c.Read()
		}
		return nil, errs.New(errCode, err_)
	}
	dataLen := binary.LittleEndian.Uint32(lenBuf)
	buf := make([]byte, dataLen)
	_, err_ = c.Conn.Read(buf)
	if err_ != nil {
		return nil, errs.New(core.ErrNetReadFail, err_)
	}
	return buf, nil
}

func (c *BanConn) Subscribe(tags ...string) {
	for _, tag := range tags {
		c.Tags[tag] = true
	}
}
func (c *BanConn) UnSubscribe(tags ...string) {
	for _, tag := range tags {
		delete(c.Tags, tag)
	}
}

/*
RunForever
监听连接发送的信息并处理。
根据消息的action：

	调用对应成员函数处理；直接传入msg_data
	或从listens中找对应的处理函数，如果精确匹配，传入msg_data，否则传入action, msg_data

服务器端和客户端都会调用此方法
*/
func (c *BanConn) RunForever() *errs.Error {
	defer c.Conn.Close()
	for {
		msg, err := c.ReadMsg()
		if err != nil {
			return err
		}
		isMatch := false
		for prefix, handle := range c.Listens {
			if strings.HasPrefix(msg.Action, prefix) {
				isMatch = true
				handle(msg.Action, msg.Data)
				break
			}
		}
		if !isMatch {
			log.Info("unhandle msg", zap.String("action", msg.Action))
		}
	}
}

func (c *BanConn) connect(lock bool) {
	if lock {
		c.m.Lock()
		defer c.m.Unlock()
		if c.Ready && btime.TimeMS()-c.RefreshMS < 2000 {
			// 连接已经刷新，跳过本次重试
			return
		}
	}
	c.Ready = false
	if c.Conn != nil {
		_ = c.Conn.Close()
		c.Conn = nil
	}
	core.Sleep(time.Second * 3)
	c.DoConnect(c)
	c.RefreshMS = btime.TimeMS()
	if c.Conn != nil {
		if c.ReInitConn != nil {
			c.ReInitConn()
		}
		c.Ready = true
	}
}

func (c *BanConn) initListens() {
	c.Listens["subscribe"] = func(s string, data interface{}) {
		var tags = make([]string, 0)
		if DecodeMsgData(data, &tags, "subscribe") {
			c.Subscribe(tags...)
		}
	}
	c.Listens["unsubscribe"] = func(s string, data interface{}) {
		var tags = make([]string, 0)
		if DecodeMsgData(data, &tags, "unsubscribe") {
			c.UnSubscribe(tags...)
		}
	}
}

func DecodeMsgData(input interface{}, out interface{}, name string) bool {
	err_ := mapstructure.Decode(input, out)
	if err_ != nil {
		msgText, _ := sonic.MarshalString(input)
		log.Error(name+" receive invalid", zap.String("msg", msgText))
		return false
	}
	return true
}

func compress(data []byte) ([]byte, *errs.Error) {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_, err_ := w.Write(data)
	if err_ != nil {
		return nil, errs.New(core.ErrCompressFail, err_)
	}
	err_ = w.Close()
	if err_ != nil {
		return nil, errs.New(core.ErrCompressFail, err_)
	}
	return b.Bytes(), nil
}

func deCompress(compressed []byte) ([]byte, *errs.Error) {
	var result bytes.Buffer
	b := bytes.NewReader(compressed)

	// 创建 zlib 解压缩器
	r, err := zlib.NewReader(b)
	if err != nil {
		return nil, errs.New(core.ErrDeCompressFail, err)
	}
	defer r.Close()

	// 将解压后的数据复制到 result 中
	_, err = io.Copy(&result, r)
	if err != nil {
		return nil, errs.New(core.ErrDeCompressFail, err)
	}

	return result.Bytes(), nil
}

func getErrType(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return core.ErrNetTimeout, "op_timeout"
		} else if opErr.Temporary() {
			return core.ErrNetTemporary, "op_temporary"
		} else if opErr.Op == "dial" {
			return core.ErrNetConnect, "op_conn_dial"
		} else {
			return core.ErrNetUnknown, "op_err"
		}
	}
	var callErr *syscall.Errno
	if errors.As(err, &callErr) {
		if errors.Is(callErr, syscall.ECONNRESET) {
			return core.ErrNetConnect, "call_reset"
		} else if errors.Is(callErr, syscall.EPIPE) {
			return core.ErrNetConnect, "call_pipe"
		} else {
			return core.ErrNetUnknown, "call_err"
		}
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return core.ErrNetTimeout, "dead_exceeded"
	}
	if errors.Is(err, io.EOF) {
		return core.ErrNetConnect, "io_eof"
	} else if errors.Is(err, io.ErrClosedPipe) {
		return core.ErrNetConnect, "pipe_closed"
	} else {
		return core.ErrNetUnknown, "net_fail"
	}
}

type ServerIO struct {
	Addr     string
	Name     string
	Conns    []IBanConn
	Data     map[string]interface{} // 缓存的数据，可供远程端访问
	DataExp  map[string]int64       // 缓存数据的过期时间戳，13位
	InitConn func(*BanConn)
}

var (
	banServer *ServerIO
)

func NewBanServer(addr, name string) *ServerIO {
	var server ServerIO
	server.Addr = addr
	server.Name = name
	server.Data = map[string]interface{}{}
	banServer = &server
	return &server
}

func (s *ServerIO) RunForever() *errs.Error {
	ln, err_ := net.Listen("tcp", s.Addr)
	if err_ != nil {
		return errs.New(core.ErrNetConnect, err_)
	}
	defer ln.Close()
	log.Info("banio started", zap.String("name", s.Name), zap.String("addr", s.Addr))
	for {
		conn_, err_ := ln.Accept()
		if err_ != nil {
			return errs.New(core.ErrNetConnect, err_)
		}
		conn := s.WrapConn(conn_)
		log.Info("receive client", zap.String("remote", conn.GetRemote()))
		s.Conns = append(s.Conns, conn)
		go func() {
			err := conn.RunForever()
			if err != nil {
				log.Error("read client fail", zap.String("remote", conn.GetRemote()),
					zap.String("err", err.Msg))
			}
		}()
	}
}

type KeyValExpire struct {
	Key        string
	Val        interface{}
	ExpireSecs int
}

type IOKeyVal struct {
	Key string
	Val interface{}
}

func (s *ServerIO) SetVal(args *KeyValExpire) {
	if args.Val == nil {
		// 删除值
		delete(s.Data, args.Key)
		return
	}
	s.Data[args.Key] = args.Val
	if args.ExpireSecs > 0 {
		s.DataExp[args.Key] = btime.TimeMS() + int64(args.ExpireSecs*1000)
	}
}

func (s *ServerIO) GetVal(key string) interface{} {
	val, ok := s.Data[key]
	if !ok {
		return nil
	}
	if exp, ok := s.DataExp[key]; ok {
		if btime.TimeMS() >= exp {
			delete(s.Data, key)
			delete(s.DataExp, key)
			return nil
		}
	}
	return val
}

func (s *ServerIO) Broadcast(msg *IOMsg) *errs.Error {
	allConns := make([]IBanConn, 0, len(s.Conns))
	curConns := make([]IBanConn, 0)
	for _, conn := range s.Conns {
		if conn.IsClosed() {
			continue
		}
		allConns = append(allConns, conn)
		if conn.HasTag(msg.Action) {
			curConns = append(curConns, conn)
		}
	}
	s.Conns = allConns
	if len(curConns) == 0 {
		return nil
	}
	raw, err_ := sonic.Marshal(*msg)
	if err_ != nil {
		return errs.New(core.ErrMarshalFail, err_)
	}
	compressed, err := compress(raw)
	if err != nil {
		return err
	}
	for _, conn := range curConns {
		go func(c IBanConn) {
			err = c.Write(compressed)
			if err != nil {
				log.Error("broadcast fail", zap.String("remote", c.GetRemote()),
					zap.String("tag", msg.Action), zap.Error(err))
			}
		}(conn)
	}
	return nil
}

func (s *ServerIO) WrapConn(conn net.Conn) *BanConn {
	res := &BanConn{
		Conn:      conn,
		Tags:      map[string]bool{},
		Listens:   map[string]ConnCB{},
		RefreshMS: btime.TimeMS(),
		Ready:     true,
		Remote:    conn.RemoteAddr().String(),
	}
	res.Listens["onGetVal"] = func(action string, data interface{}) {
		key := fmt.Sprintf("%v", data)
		val := s.GetVal(key)
		err := res.WriteMsg(&IOMsg{Action: "onGetValRes", Data: IOKeyVal{
			Key: key,
			Val: val,
		}})
		if err != nil {
			log.Error("write val res fail", zap.Error(err))
		}
	}
	res.Listens["onSetVal"] = func(action string, data interface{}) {
		var args KeyValExpire
		if DecodeMsgData(data, &args, "onSetVal") {
			s.SetVal(&args)
		}
	}
	res.initListens()
	if s.InitConn != nil {
		s.InitConn(res)
	}
	return res
}

type ClientIO struct {
	BanConn
	Addr  string
	waits map[string]chan interface{}
}

func NewClientIO(addr string) (*ClientIO, *errs.Error) {
	conn, err_ := net.Dial("tcp", addr)
	if err_ != nil {
		return nil, errs.New(core.ErrNetConnect, err_)
	}
	res := &ClientIO{
		Addr: addr,
		BanConn: BanConn{
			Conn:      conn,
			Tags:      map[string]bool{},
			Remote:    conn.RemoteAddr().String(),
			Listens:   map[string]ConnCB{},
			RefreshMS: btime.TimeMS(),
			Ready:     true,
		},
		waits: map[string]chan interface{}{},
	}
	res.Listens["onGetValRes"] = func(_ string, data interface{}) {
		var val IOKeyVal
		if DecodeMsgData(data, &val, "onGetValRes") {
			out, ok := res.waits[val.Key]
			if !ok {
				return
			}
			out <- val.Val
		}
	}
	res.DoConnect = func(c *BanConn) {
		for {
			cn, err_ := net.Dial("tcp", addr)
			if err_ != nil {
				log.Error("connect fail, sleep 10s and retry..", zap.String("addr", addr))
				core.Sleep(time.Second * 10)
				continue
			}
			c.Conn = cn
			return
		}
	}
	banClient = res
	return res, nil
}

const (
	readTimeout = 120
)

func (c *ClientIO) GetVal(key string, timeout int) (interface{}, *errs.Error) {
	err := c.WriteMsg(&IOMsg{
		Action: "onGetVal",
		Data:   key,
	})
	if err != nil {
		return nil, err
	}
	if timeout == 0 {
		timeout = readTimeout
	}
	out := make(chan interface{})
	c.waits[key] = out
	var res interface{}
	select {
	case res = <-out:
	case <-time.After(time.Second * time.Duration(timeout)):
		close(out)
		delete(c.waits, key)
	}
	return res, nil
}

func (c *ClientIO) SetVal(args *KeyValExpire) *errs.Error {
	return c.WriteMsg(&IOMsg{
		Action: "onSetVal",
		Data:   *args,
	})
}

var (
	banClient *ClientIO
)

func GetServerData(key string) (interface{}, *errs.Error) {
	if banServer != nil {
		data := banServer.GetVal(key)
		return data, nil
	}
	if banClient == nil {
		return nil, errs.NewMsg(core.ErrRunTime, "banClient not load")
	}
	return banClient.GetVal(key, 0)
}

func SetServerData(args *KeyValExpire) *errs.Error {
	if banServer != nil {
		banServer.SetVal(args)
		return nil
	}
	if banClient == nil {
		return errs.NewMsg(core.ErrRunTime, "banClient not load")
	}
	return banClient.SetVal(args)
}

func GetNetLock(key string, timeout int) (int32, *errs.Error) {
	lockKey := "lock_" + key
	val, err := GetServerData(lockKey)
	if err != nil {
		return 0, err
	}
	lockVal := rand.Int31()
	if val == nil {
		err = SetServerData(&KeyValExpire{Key: lockKey, Val: lockVal})
		return lockVal, err
	}
	if timeout == 0 {
		timeout = 30
	}
	stopAt := btime.Time() + float64(timeout)
	for btime.Time() < stopAt {
		core.Sleep(time.Microsecond * 10)
		val, err = GetServerData(lockKey)
		if err != nil {
			return 0, err
		}
		if val == nil {
			err = SetServerData(&KeyValExpire{Key: lockKey, Val: lockVal})
			return lockVal, err
		}
	}
	return 0, errs.NewMsg(core.ErrTimeout, "GetNetLock for %s", key)
}

func DelNetLock(key string, lockVal int32) *errs.Error {
	lockKey := "lock_" + key
	val, err := GetServerData(lockKey)
	if err != nil {
		return err
	}
	var valInt = int32(0)
	_ = mapstructure.Decode(val, &valInt)
	if valInt == lockVal {
		return SetServerData(&KeyValExpire{Key: lockKey, Val: nil})
	}
	log.Info("del lock fail", zap.Int32("val", valInt), zap.Int32("exp", lockVal))
	return nil
}
