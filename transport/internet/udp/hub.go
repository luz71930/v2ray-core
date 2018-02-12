package udp

import (
	"v2ray.com/core/common/buf"
	"v2ray.com/core/common/dice"
	"v2ray.com/core/common/net"
)

// Payload represents a single UDP payload.
type Payload struct {
	payload      *buf.Buffer
	source       net.Destination
	originalDest net.Destination
}

// PayloadHandler is function to handle Payload.
type PayloadHandler func(payload *buf.Buffer, source net.Destination, originalDest net.Destination)

// PayloadQueue is a queue of Payload.
type PayloadQueue struct {
	queue    []chan Payload
	callback PayloadHandler
}

// NewPayloadQueue returns a new PayloadQueue.
func NewPayloadQueue(option ListenOption) *PayloadQueue {
	queue := &PayloadQueue{
		callback: option.Callback,
		queue:    make([]chan Payload, option.Concurrency),
	}
	for i := range queue.queue {
		queue.queue[i] = make(chan Payload, 64)
		go queue.Dequeue(queue.queue[i])
	}
	return queue
}

// Enqueue adds the payload to the end of this queue.
func (q *PayloadQueue) Enqueue(payload Payload) {
	size := len(q.queue)
	idx := 0
	if size > 1 {
		idx = dice.Roll(size)
	}
	for i := 0; i < size; i++ {
		select {
		case q.queue[idx%size] <- payload:
			return
		default:
			idx++
		}
	}
}

func (q *PayloadQueue) Dequeue(queue <-chan Payload) {
	for payload := range queue {
		q.callback(payload.payload, payload.source, payload.originalDest)
	}
}

func (q *PayloadQueue) Close() error {
	for _, queue := range q.queue {
		close(queue)
	}
	return nil
}

type ListenOption struct {
	Callback            PayloadHandler
	ReceiveOriginalDest bool
	Concurrency         int
}

type Hub struct {
	conn   *net.UDPConn
	queue  *PayloadQueue
	option ListenOption
}

func ListenUDP(address net.Address, port net.Port, option ListenOption) (*Hub, error) {
	if option.Concurrency < 1 {
		option.Concurrency = 1
	}
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   address.IP(),
		Port: int(port),
	})
	if err != nil {
		return nil, err
	}
	newError("listening UDP on ", address, ":", port).WriteToLog()
	if option.ReceiveOriginalDest {
		rawConn, err := udpConn.SyscallConn()
		if err != nil {
			return nil, newError("failed to get fd").Base(err)
		}
		err = rawConn.Control(func(fd uintptr) {
			if err := SetOriginalDestOptions(int(fd)); err != nil {
				newError("failed to set socket options").Base(err).WriteToLog()
			}
		})
		if err != nil {
			return nil, newError("failed to control socket").Base(err)
		}
	}
	hub := &Hub{
		conn:   udpConn,
		queue:  NewPayloadQueue(option),
		option: option,
	}
	go hub.start()
	return hub, nil
}

func (h *Hub) Close() error {
	h.conn.Close()
	return nil
}

func (h *Hub) WriteTo(payload []byte, dest net.Destination) (int, error) {
	return h.conn.WriteToUDP(payload, &net.UDPAddr{
		IP:   dest.Address.IP(),
		Port: int(dest.Port),
	})
}

func (h *Hub) start() {
	oobBytes := make([]byte, 256)

	for {
		buffer := buf.New()
		var noob int
		var addr *net.UDPAddr
		err := buffer.AppendSupplier(func(b []byte) (int, error) {
			n, nb, _, a, e := ReadUDPMsg(h.conn, b, oobBytes)
			noob = nb
			addr = a
			return n, e
		})

		if err != nil {
			newError("failed to read UDP msg").Base(err).WriteToLog()
			buffer.Release()
			break
		}

		payload := Payload{
			payload: buffer,
		}
		payload.source = net.UDPDestination(net.IPAddress(addr.IP), net.Port(addr.Port))
		if h.option.ReceiveOriginalDest && noob > 0 {
			payload.originalDest = RetrieveOriginalDest(oobBytes[:noob])
			if payload.originalDest.IsValid() {
				newError("UDP original destination: ", payload.originalDest).AtDebug().WriteToLog()
			} else {
				newError("failed to read UDP original destination").WriteToLog()
			}
		}
		h.queue.Enqueue(payload)
	}
	h.queue.Close()
}

func (h *Hub) Addr() net.Addr {
	return h.conn.LocalAddr()
}
