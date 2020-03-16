/*************************************************************************
 * Copyright 2017 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/
// +build windows

package winevent

import (
	"bytes"
	//"fmt"
	"sync"

	"golang.org/x/sys/windows"

	"github.com/gravwell/winevent/v3/wineventlog"
)

const (
	defaultBuffSize  = 4 * 1024 * 1024 //4MB  Sure... why not
	maxHandleRequest = 128
	minHandleRequest = 2                //this CANNOT be less than 2, or you will fall into an infinite loop HAMMERING the kernel
	maxBuffSize      = 32 * 1024 * 1024 //a 32MB message is stupid
)

type EventStreamHandle struct {
	params    EventStreamParams
	sigEvent  windows.Handle
	subHandle wineventlog.EvtHandle
	buff      []byte
	last      uint64
	mtx       *sync.Mutex
}

func NewStream(param EventStreamParams, last uint64) (e *EventStreamHandle, err error) {
	e = &EventStreamHandle{
		params: param,
		buff:   make([]byte, defaultBuffSize),
		mtx:    &sync.Mutex{},
		last:   last,
	}
	if err = e.open(); err != nil {
		e = nil
	}
	return
}

func (e *EventStreamHandle) open() error {
	sigEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return err
	}
	query, err := genQuery(e.params)
	if err != nil {
		windows.CloseHandle(sigEvent)
		return err
	}
	//we build our bookmark
	var bmk wineventlog.EvtHandle
	flags := wineventlog.EvtSubscribeStartAtOldestRecord
	if e.last > 0 {
		flags = wineventlog.EvtSubscribeStartAfterBookmark
		if bmk, err = wineventlog.CreateBookmarkFromRecordID(e.params.Channel, e.last); err != nil {
			return err
		}
		defer wineventlog.Close(bmk)
	}

	subHandle, err := wineventlog.Subscribe(
		0, //localhost session
		sigEvent,
		``,    // channel is in the query
		query, //query has the reachback parameter
		bmk,
		flags)
	if err != nil {
		windows.CloseHandle(sigEvent)
		return err
	}

	e.mtx.Lock()
	e.sigEvent = sigEvent
	e.subHandle = subHandle
	e.mtx.Unlock()
	return nil
}

func (e *EventStreamHandle) close() (err error) {
	if err = windows.CloseHandle(e.sigEvent); err != nil {
		wineventlog.Close(e.subHandle) //just close the subhandle
	} else {
		err = wineventlog.Close(e.subHandle)
	}
	return
}

func (e *EventStreamHandle) reset() (err error) {
	e.mtx.Lock()
	e.close()
	err = e.open()
	e.mtx.Unlock()
	return
}

//getHandles will iterate on the call to EventHandles, we do this because on big event log entries the kernel throws
//RPC_S_INVALID_BOUND which is basically a really shitty way to say "i can't give you all the handles due to size"
func (e *EventStreamHandle) getHandles(min, max int) (evtHnds []wineventlog.EvtHandle, err error) {
	for cnt := max; cnt >= min; cnt = cnt / 2 {
		evtHnds, err = wineventlog.EventHandles(e.subHandle, cnt)
		switch err {
		case nil:
			return //got a good read
		case wineventlog.ERROR_NO_MORE_ITEMS:
			err = nil
			return //empty
		case wineventlog.RPC_S_INVALID_BOUND:
			//our buffer isn't big enough, reset the handle and try again
			if err = e.reset(); err != nil {
				return
			}
			//we will retry
		default:
			return
		}
	}
	//if we hit here, then our buffer is not big enough to handle two entries
	evtHnds, err = wineventlog.EventHandles(e.subHandle, 1)
	return
}

type RenderedEvent struct {
	Buff []byte
	ID   uint64
}

func (e *EventStreamHandle) Read() (ents []RenderedEvent, err error) {
	var bmk wineventlog.EvtHandle
	var evtHandles []wineventlog.EvtHandle
	if evtHandles, err = e.getHandles(minHandleRequest, maxHandleRequest); err != nil {
		return
	} else if len(evtHandles) == 0 {
		return
	}
	if bmk, err = wineventlog.CreateBookmark(); err != nil {
		for _, v := range evtHandles {
			wineventlog.Close(v)
		}
		return
	}
	defer wineventlog.Close(bmk)

	bb := bytes.NewBuffer(nil)
	for _, h := range evtHandles {
		var re RenderedEvent
		bb.Reset()
		if err = wineventlog.RenderEventSimple(h, e.buff, bb); err != nil {
			ents = nil
			break
		}
		re.Buff = append(re.Buff, bb.Bytes()...)
		bb.Reset()
		if err = wineventlog.UpdateBookmarkFromEvent(bmk, h); err != nil {
			ents = nil
			break
		} else if re.ID, err = wineventlog.GetRecordIDFromBookmark(bmk, e.buff, bb); err != nil {
			ents = nil
			break
		}
		ents = append(ents, re)
	}
	for _, h := range evtHandles {
		wineventlog.Close(h)
	}
	return
}

func (e *EventStreamHandle) Name() (s string) {
	e.mtx.Lock()
	s = e.params.Name
	e.mtx.Unlock()
	return
}

func (e *EventStreamHandle) Last() (l uint64) {
	e.mtx.Lock()
	l = e.last
	e.mtx.Unlock()
	return
}

func (e *EventStreamHandle) SetLast(v uint64) {
	e.mtx.Lock()
	e.last = v
	e.mtx.Unlock()
}

func (e *EventStreamHandle) Close() (err error) {
	e.mtx.Lock()
	err = e.close()
	e.mtx.Unlock()
	return
}

func ChannelAvailable(c string) (bool, error) {
	chs, err := wineventlog.Channels()
	if err != nil {
		return false, err
	}
	for i := range chs {
		if chs[i] == c {
			return true, nil
		}
	}
	return false, nil
}

/*
func printChannels() {
	chs, err := wineventlog.Channels()
	if err != nil {
		fmt.Println("Channels error", err)
		return
	}
	for i := range chs {
		fmt.Println("channel", i, chs[i])
	}
}
*/

func genQuery(p EventStreamParams) (string, error) {
	return wineventlog.Query{
		Log:         p.Channel,
		IgnoreOlder: p.ReachBack,
		Level:       p.Levels,
		Provider:    p.Providers,
		EventID:     p.EventIDs, //black list and white list of event IDs (add - in front to remove one) blank is all
	}.Build()
}
