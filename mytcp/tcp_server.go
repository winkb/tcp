package mytcp

import (
	"fmt"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

type CloseCallback func(conn *TcpConn, isServer bool, isClient bool)
type ReceiveCallback func(conn *TcpConn, bt []byte)

type ITcpServer interface {
	Shutdown()
	Send(conn *TcpConn, v []byte)
	OnReceive(f ReceiveCallback)
	OnClose(f CloseCallback)
	Start() (wg *sync.WaitGroup, err error)
	LoopAccept(f func(conn net.Conn))
	ConsumeInput(conn *TcpConn)
	ConsumeOutput(conn *TcpConn)
	LoopRead(conn *TcpConn)
}

var _ ITcpServer = (*tcpServer)(nil)

type tcpServer struct {
	listener        net.Listener
	closeCallback   CloseCallback
	receiveCallback ReceiveCallback
	addr            string
	conns           sync.Map
	lastId          uint32
	stop            int
	lock            sync.Mutex
}

type TcpConn struct {
	conn     net.Conn
	id       uint32
	input    chan []byte
	output   chan []byte
	waitConn chan bool
}

func NewTcpServer(port string) *tcpServer {
	return &tcpServer{
		listener: nil,
		closeCallback: func(conn *TcpConn, isServer bool, isClient bool) {
		},
		receiveCallback: func(conn *TcpConn, bt []byte) {
		},
		addr:   ":" + port,
		conns:  sync.Map{},
		lastId: 0,
		stop:   0,
		lock:   sync.Mutex{},
	}
}

func (l *tcpServer) LoopAccept(f func(conn net.Conn)) {
	for {
		accept, err := l.listener.Accept()
		if err != nil {
			if _, ok := err.(*net.OpError); ok {
				fmt.Println("server shutdown")
				return
			}

			log.Err(errors.Wrap(err, "accept"))
			return
		}

		l.lock.Lock()
		if l.stop != 0 {
			fmt.Println("server is stop")
			continue
		}

		f(accept)
		l.lock.Unlock()
	}
}

func (l *tcpServer) getConnAutoIncId() uint32 {
	for {
		val := atomic.LoadUint32(&l.lastId)
		old := val
		val += 1
		if atomic.CompareAndSwapUint32(&l.lastId, old, val) {
			return val
		}
	}
}

func (l *tcpServer) getConnById(id uint32) (conn *TcpConn, ok bool) {
	v, o := l.conns.Load(id)
	if !o {
		return
	}

	conn, ok = v.(*TcpConn)

	return
}

func (l *tcpServer) saveConn(id uint32, conn *TcpConn) {
	l.conns.Store(id, conn)
}

func (l *tcpServer) ConsumeOutput(conn *TcpConn) {
	for {
		select {
		case <-conn.waitConn:
			return
		case msg := <-conn.output:
			id := conn.id
			fmt.Println("output id", id, "msg", string(msg))
			l.handelReceive(conn, msg)
		}
	}
}

func (l *tcpServer) ConsumeInput(conn *TcpConn) {
	for {
		select {
		case <-conn.waitConn:
			return
		case msg := <-conn.input:
			l.lock.Lock()
			if l.stop != 0 {
				continue
			}

			id := conn.id
			_, err := conn.conn.Write(msg)
			if err != nil {
				log.Err(errors.Wrapf(err, "conn %d write err", id))
				continue
			}

			log.Print("input id", id, "msg", string(msg))

			l.lock.Unlock()
		}
	}
}

func (l *tcpServer) LoopRead(conn *TcpConn) {
	for {
		select {
		case <-conn.waitConn:
			return
		case <-conn.output:
		default:
			bt := make([]byte, 1024)
			n, err := conn.conn.Read(bt)
			if err != nil {
				if err == io.EOF {
					l.handelReadClose(conn, false, true)
					return
				}

				if _, ok := err.(*net.OpError); ok {
					l.handelReadClose(conn, true, false)
					return
				}

				log.Err(errors.Wrap(err, "read"))
				return
			}

			bt = bt[:n]
			conn.output <- bt
		}
	}
}

func (l *tcpServer) handelReadClose(conn *TcpConn, isServer bool, isClient bool) {
	close(conn.waitConn)
	if l.closeCallback != nil {
		l.closeCallback(conn, isServer, isClient)
	}
}

func (l *tcpServer) handelReceive(conn *TcpConn, bt []byte) {
	if l.receiveCallback != nil {
		l.receiveCallback(conn, bt)
	}
}

func (l *tcpServer) Shutdown() {
	l.lock.Lock()
	defer l.lock.Unlock()

	if l.stop != 0 {
		return
	}

	l.stop = 2
	l.conns.Range(func(key, value any) bool {
		v, ok := value.(*TcpConn)
		if ok {
			_ = v.conn.Close()
		}
		return true
	})

	l.listener.Close()
}

func (l *tcpServer) Send(conn *TcpConn, v []byte) {
	conn.input <- v
}

func (l *tcpServer) SendById(id uint32, v []byte) {
	conn, ok := l.getConnById(id)
	if !ok {
		log.Err(errors.Errorf("not found conn %d", id))
		return
	}

	l.Send(conn, v)
}

func (l *tcpServer) OnReceive(f ReceiveCallback) {
	l.receiveCallback = f
}

func (l *tcpServer) OnClose(f CloseCallback) {
	l.closeCallback = f
}

func (l *tcpServer) Start() (wg *sync.WaitGroup, err error) {
	wg = &sync.WaitGroup{}
	// conn server
	err = l.listen()
	if err != nil {
		return
	}
	// read
	MyGoWg(wg, "conn_accept", func() {
		l.LoopAccept(func(conn net.Conn) {
			// 注意 这里不能阻塞 lock,因为accept，有lock判断

			newId := l.getConnAutoIncId()
			myConn := &TcpConn{
				conn:     conn,
				id:       newId,
				input:    make(chan []byte),
				output:   make(chan []byte),
				waitConn: make(chan bool),
			}

			MyGoWg(wg, fmt.Sprintf("%d_conn_read", newId), func() {
				l.LoopRead(myConn)
			})

			MyGoWg(wg, fmt.Sprintf("%d_conn_consume_input", newId), func() {
				l.ConsumeInput(myConn)
			})

			MyGoWg(wg, fmt.Sprintf("%d_conn_consume_output", newId), func() {
				l.ConsumeOutput(myConn)
			})

			fmt.Println(conn.RemoteAddr().String() + "conn success")

			l.saveConn(newId, myConn)
		})
	})

	fmt.Println("start server " + l.addr)

	return
}

func (l *tcpServer) listen() (err error) {
	var conn net.Listener
	conn, err = net.Listen("tcp", l.addr)
	if err != nil {
		err = errors.Wrap(err, "dial:"+l.addr)
		return
	}
	l.listener = conn
	return
}

func (l *tcpServer) Broadcast(bt []byte) {
	l.conns.Range(func(key, value any) bool {
		v, ok := value.(*TcpConn)
		if ok {
			v.input <- bt
		}
		return true
	})
}
