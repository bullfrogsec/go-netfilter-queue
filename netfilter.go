/*
   Copyright 2014 Krishna Raman <kraman@gmail.com>

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

/*
Go bindings for libnetfilter_queue

This library provides access to packets in the IPTables netfilter queue (NFQUEUE).
The libnetfilter_queue library is part of the http://netfilter.org/projects/libnetfilter_queue/ project.
*/
package netfilter

/*
#cgo pkg-config: libnetfilter_queue
#cgo CFLAGS: -Wall -I/usr/include
#cgo LDFLAGS: -L/usr/lib64/

#include "netfilter.h"
*/
import "C"

import (
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
)

// Verdict for a packet
type Verdict C.uint

type NFPacket struct {
	Packet gopacket.Packet
	Mark   uint32
	qh     *C.struct_nfq_q_handle
	id     C.uint32_t
}

// Set the verdict for the packet
func (p *NFPacket) SetVerdict(v Verdict) {
	C.nfq_set_verdict(p.qh, p.id, C.uint(v), 0, nil)
}

// Set the verdict for the packet with a mark
func (p *NFPacket) SetVerdictMark(v Verdict, mark uint32) {
	C.nfq_set_verdict2(p.qh, p.id, C.uint(v), C.uint32_t(mark), 0, nil)
}

// Set the verdict for the packet (in the case of requeue)
func (p *NFPacket) SetRequeueVerdict(newQueueId uint16) {
	v := uint(NF_QUEUE)
	q := (uint(newQueueId) << 16)
	v = v | q
	C.nfq_set_verdict(p.qh, p.id, C.uint(v), 0, nil)
}

// Set the verdict for the packet AND provide new packet content for injection
func (p *NFPacket) SetVerdictWithPacket(v Verdict, packet []byte) {
	C.nfq_set_verdict(
		p.qh,
		p.id,
		C.uint(v),
		C.uint(len(packet)),
		(*C.uchar)(unsafe.Pointer(&packet[0])),
	)
}

type NFQueue struct {
	h       *C.struct_nfq_handle
	qh      *C.struct_nfq_q_handle
	fd      C.int
	packets chan NFPacket
	idx     uint32
}

const (
	AF_INET  = 2
	AF_INET6 = 10

	NF_DROP   Verdict = 0
	NF_ACCEPT Verdict = 1
	NF_STOLEN Verdict = 2
	NF_QUEUE  Verdict = 3
	NF_REPEAT Verdict = 4
	NF_STOP   Verdict = 5

	NF_DEFAULT_PACKET_SIZE uint32 = 0xffff

	ipv4version = 0x40
)

var theTable = make(map[uint32]*chan NFPacket, 0)
var theTabeLock sync.RWMutex

// Create and bind to queue specified by queueId
func NewNFQueue(queueId uint16, maxPacketsInQueue uint32, packetSize uint32) (*NFQueue, error) {
	var nfq = NFQueue{}
	var err error
	var ret C.int

	if nfq.h, err = C.nfq_open(); err != nil {
		return nil, fmt.Errorf("Error opening NFQueue handle: %v\n", err)
	}

	if ret, err = C.nfq_unbind_pf(nfq.h, AF_INET); err != nil || ret < 0 {
		return nil, fmt.Errorf("Error unbinding existing NFQ handler from AF_INET protocol family: %v\n", err)
	}

	if ret, err = C.nfq_unbind_pf(nfq.h, AF_INET6); err != nil || ret < 0 {
		return nil, fmt.Errorf("Error unbinding existing NFQ handler from AF_INET6 protocol family: %v\n", err)
	}

	if ret, err := C.nfq_bind_pf(nfq.h, AF_INET); err != nil || ret < 0 {
		return nil, fmt.Errorf("Error binding to AF_INET protocol family: %v\n", err)
	}

	if ret, err := C.nfq_bind_pf(nfq.h, AF_INET6); err != nil || ret < 0 {
		return nil, fmt.Errorf("Error binding to AF_INET6 protocol family: %v\n", err)
	}

	nfq.packets = make(chan NFPacket, maxPacketsInQueue)
	nfq.idx = uint32(time.Now().UnixNano())
	theTabeLock.Lock()
	theTable[nfq.idx] = &nfq.packets
	theTabeLock.Unlock()
	if nfq.qh, err = C.CreateQueue(nfq.h, C.u_int16_t(queueId), C.u_int32_t(nfq.idx)); err != nil || nfq.qh == nil {
		C.nfq_close(nfq.h)
		return nil, fmt.Errorf("Error binding to queue: %v\n", err)
	}

	if ret, err = C.nfq_set_queue_maxlen(nfq.qh, C.u_int32_t(maxPacketsInQueue)); err != nil || ret < 0 {
		C.nfq_destroy_queue(nfq.qh)
		C.nfq_close(nfq.h)
		return nil, fmt.Errorf("Unable to set max packets in queue: %v\n", err)
	}

	if C.nfq_set_mode(nfq.qh, C.u_int8_t(2), C.uint(packetSize)) < 0 {
		C.nfq_destroy_queue(nfq.qh)
		C.nfq_close(nfq.h)
		return nil, fmt.Errorf("Unable to set packets copy mode: %v\n", err)
	}

	if nfq.fd, err = C.nfq_fd(nfq.h); err != nil {
		C.nfq_destroy_queue(nfq.qh)
		C.nfq_close(nfq.h)
		return nil, fmt.Errorf("Unable to get queue file-descriptor. %v\n", err)
	}

	go nfq.run()

	return &nfq, nil
}

// Unbind and close the queue
func (nfq *NFQueue) Close() {
	C.nfq_destroy_queue(nfq.qh)
	C.nfq_close(nfq.h)
	theTabeLock.Lock()
	close(nfq.packets)
	delete(theTable, nfq.idx)
	theTabeLock.Unlock()
}

// Get the channel for packets
func (nfq *NFQueue) GetPackets() <-chan NFPacket {
	return nfq.packets
}

func (nfq *NFQueue) run() {
	if errno := C.Run(nfq.h, nfq.fd); errno != 0 {
		fmt.Fprintf(os.Stderr, "Terminating, unable to receive packet due to errno=%d\n", errno)
	}
}

//export go_callback
func go_callback(packetId C.uint32_t, data *C.uchar, length C.int, mark C.uint32_t, idx uint32, qh *C.struct_nfq_q_handle) {
	xdata := C.GoBytes(unsafe.Pointer(data), length)

	var packet gopacket.Packet
	if xdata[0]&0xf0 == ipv4version {
		packet = gopacket.NewPacket(xdata, layers.LayerTypeIPv4, gopacket.DecodeOptions{Lazy: true, NoCopy: true})
	} else {
		packet = gopacket.NewPacket(xdata, layers.LayerTypeIPv6, gopacket.DecodeOptions{Lazy: true, NoCopy: true})
	}

	p := NFPacket{
		Packet: packet,
		qh:     qh,
		id:     packetId,
		Mark:   uint32(mark),
	}

	theTabeLock.RLock()
	cb, ok := theTable[idx]
	theTabeLock.RUnlock()
	if !ok {
		fmt.Fprintf(os.Stderr, "Dropping, unexpectedly due to bad idx=%d\n", idx)
		p.SetVerdict(NF_DROP)
	}

	// Nonblocking write of packet to queue channel
	select {
	case *cb <- p:
	default:
		fmt.Fprintf(os.Stderr, "Dropping, unexpectedly due to no recv, idx=%d\n", idx)
		p.SetVerdict(NF_DROP)
	}
}
