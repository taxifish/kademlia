package kademlia

import (
	"errors"
	"net"
	"strconv"
	"sync"

	"github.com/anacrolix/utp"
)

var (
	errorValueNotFound = errors.New("Value not found")
)

// Network TODO
type networking interface {
	sendMessage(*message, int64, bool) (*expectedResponse, error)
	getMessage() chan (*message)
	messagesFin()
	timersFin()
	getDisconnect() chan (int)
	init(self *NetworkNode)
	createSocket(host string, port string) error
	listen() error
	disconnect() error
	cancelResponse(*expectedResponse)
	isInitialized() bool
}

type realNetworking struct {
	socket        *utp.Socket
	sendChan      chan (*message)
	recvChan      chan (*message)
	dcStartChan   chan (int)
	dcEndChan     chan (int)
	dcTimersChan  chan (int)
	dcMessageChan chan (int)
	address       *net.UDPAddr
	connection    *net.UDPConn
	mutex         *sync.Mutex
	connected     bool
	initialized   bool
	responseMap   map[int64]*expectedResponse
	aliveConns    *sync.WaitGroup
	self          *NetworkNode
}

type expectedResponse struct {
	ch    chan (*message)
	query *message
	node  *NetworkNode
	id    int64
}

func (rn *realNetworking) init(self *NetworkNode) {
	rn.self = self
	rn.mutex = &sync.Mutex{}
	rn.sendChan = make(chan (*message))
	rn.recvChan = make(chan (*message))
	rn.dcStartChan = make(chan (int), 10)
	rn.dcEndChan = make(chan (int))
	rn.dcTimersChan = make(chan (int))
	rn.dcMessageChan = make(chan (int))
	rn.responseMap = make(map[int64]*expectedResponse)
	rn.aliveConns = &sync.WaitGroup{}
	rn.connected = false
	rn.initialized = true
}

func (rn *realNetworking) isInitialized() bool {
	return rn.initialized
}

func (rn *realNetworking) getMessage() chan (*message) {
	return rn.recvChan
}

func (rn *realNetworking) messagesFin() {
	rn.dcMessageChan <- 1
}

func (rn *realNetworking) getDisconnect() chan (int) {
	return rn.dcStartChan
}

func (rn *realNetworking) timersFin() {
	rn.dcTimersChan <- 1
}

func (rn *realNetworking) createSocket(host string, port string) error {
	rn.mutex.Lock()
	defer rn.mutex.Unlock()
	if rn.connected {
		return errors.New("already connected")
	}
	socket, err := utp.NewSocket("udp", host+":"+port)
	if err != nil {
		return err
	}

	rn.connected = true

	rn.socket = socket

	return nil
}

func (rn *realNetworking) sendMessage(msg *message, id int64, expectResponse bool) (*expectedResponse, error) {
	rn.mutex.Lock()
	defer rn.mutex.Unlock()
	msg.ID = id

	conn, err := utp.Dial(msg.Receiver.IP.String() + ":" + strconv.Itoa(msg.Receiver.Port))
	if err != nil {
		return nil, err
	}

	data, err := serializeMessage(msg)
	if err != nil {
		return nil, err
	}

	_, err = conn.Write(data)
	if err != nil {
		return nil, err
	}

	if expectResponse {
		expectedResponse := &expectedResponse{
			ch:    make(chan (*message)),
			node:  msg.Receiver,
			query: msg,
			id:    id,
		}
		rn.responseMap[id] = expectedResponse
		return expectedResponse, nil
	}

	return nil, nil
}

func (rn *realNetworking) cancelResponse(res *expectedResponse) {
	rn.mutex.Lock()
	defer rn.mutex.Unlock()
	close(rn.responseMap[res.id].ch)
	delete(rn.responseMap, res.id)
}

func (rn *realNetworking) disconnect() error {
	rn.mutex.Lock()
	defer rn.mutex.Unlock()
	if !rn.connected {
		return errors.New("not connected")
	}
	rn.dcStartChan <- 1
	rn.dcStartChan <- 1
	<-rn.dcTimersChan
	<-rn.dcMessageChan
	close(rn.sendChan)
	close(rn.recvChan)
	close(rn.dcTimersChan)
	close(rn.dcMessageChan)
	err := rn.socket.CloseNow()
	rn.connected = false
	rn.initialized = false
	close(rn.dcEndChan)
	return err
}

func (rn *realNetworking) listen() error {
	for {
		conn, err := rn.socket.Accept()
		if err != nil {
			rn.disconnect()
			<-rn.dcEndChan
			return err
		}

		go func(conn net.Conn) {
			for {
				// Wait for messages
				msg, err := deserializeMessage(conn)
				if err != nil {
					// TODO should we penalize this node somehow ? Ban it ?
					return
				}

				isPing := msg.Type == messageTypePing
				if !areNodesEqual(msg.Receiver, rn.self, isPing) {
					// TODO should we penalize this node somehow ? Ban it ?
					continue
				}

				rn.mutex.Lock()
				if rn.connected {
					if msg.IsResponse && rn.responseMap[msg.ID] != nil {
						if !areNodesEqual(rn.responseMap[msg.ID].node, msg.Sender, isPing) {
							// TODO should we penalize this node somehow ? Ban it ?
							rn.mutex.Unlock()
							continue
						}

						if msg.Type != rn.responseMap[msg.ID].query.Type {
							close(rn.responseMap[msg.ID].ch)
							delete(rn.responseMap, msg.ID)
							rn.mutex.Unlock()
							continue
						}

						if !msg.IsResponse {
							close(rn.responseMap[msg.ID].ch)
							delete(rn.responseMap, msg.ID)
							rn.mutex.Unlock()
							continue
						}

						rn.responseMap[msg.ID].ch <- msg
						close(rn.responseMap[msg.ID].ch)
						delete(rn.responseMap, msg.ID)
					} else {
						rn.recvChan <- msg
					}
				}
				rn.mutex.Unlock()
			}
		}(conn)
	}
}
