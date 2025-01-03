// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package pulsar

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/apache/pulsar-client-go/pulsar/log"
	"github.com/sirupsen/logrus"
)

type cancelReader struct {
	reader     Reader
	cancelFunc context.CancelFunc
}

type TableViewImpl struct {
	client  *client
	options TableViewOptions

	dataMu sync.Mutex
	data   map[string]interface{}

	readersMu     sync.Mutex
	cancelReaders map[string]cancelReader

	listenersMu sync.Mutex
	listeners   []func(string, interface{}) error

	logger   log.Logger
	closed   bool
	closedCh chan struct{}
}

func newTableView(client *client, options TableViewOptions) (TableView, error) {
	if options.Topic == "" {
		return nil, newError(TopicNotFound, "topic is required")
	}

	if options.Schema != nil && options.SchemaValueType == nil {
		return nil, newError(InvalidConfiguration, "SchemaValueType is required when Schema is present")
	}

	var logger log.Logger
	if options.Logger != nil {
		logger = options.Logger
	} else {
		logger = log.NewLoggerWithLogrus(logrus.StandardLogger())
	}

	if options.AutoUpdatePartitionsInterval == 0 {
		options.AutoUpdatePartitionsInterval = time.Minute
	}

	tv := TableViewImpl{
		client:        client,
		options:       options,
		data:          make(map[string]interface{}),
		cancelReaders: make(map[string]cancelReader),
		logger:        logger,
		closedCh:      make(chan struct{}),
	}

	// Do an initial round of partition update check to make sure we can populate the partition readers
	if err := tv.partitionUpdateCheck(); err != nil {
		return nil, err
	}
	go tv.periodicPartitionUpdateCheck()

	return &tv, nil
}

func (tv *TableViewImpl) partitionUpdateCheck() error {
	partitionsArray, err := tv.client.TopicPartitions(tv.options.Topic)
	if err != nil {
		return fmt.Errorf("tv.client.TopicPartitions(%s) failed: %w", tv.options.Topic, err)
	}

	partitions := make(map[string]bool, len(partitionsArray))
	for _, partition := range partitionsArray {
		partitions[partition] = true
	}

	tv.readersMu.Lock()
	defer tv.readersMu.Unlock()

	for partition, cancelReader := range tv.cancelReaders {
		if _, ok := partitions[partition]; !ok {
			cancelReader.cancelFunc()
			cancelReader.reader.Close()
			delete(tv.cancelReaders, partition)
		}
	}

	for partition := range partitions {
		if _, ok := tv.cancelReaders[partition]; !ok {
			reader, err := newReader(tv.client, ReaderOptions{
				Topic:          partition,
				StartMessageID: EarliestMessageID(),
				ReadCompacted:  true,
				// TODO: Pooling?
				Schema: tv.options.Schema,
			})
			if err != nil {
				return fmt.Errorf("create new reader failed for %s: %w", partition, err)
			}
			for reader.HasNext() {
				msg, err := reader.Next(context.Background())
				if err != nil {
					tv.logger.Errorf("read next message failed for %s: %v", partition, err)
				}
				if msg != nil {
					tv.handleMessage(msg)
				}
			}
			ctx, cancelFunc := context.WithCancel(context.Background())
			tv.cancelReaders[partition] = cancelReader{
				reader:     reader,
				cancelFunc: cancelFunc,
			}
			go tv.watchReaderForNewMessages(ctx, reader)
		}
	}

	return nil
}

func (tv *TableViewImpl) periodicPartitionUpdateCheck() {
	for {
		if err := tv.partitionUpdateCheck(); err != nil {
			tv.logger.Errorf("failed to check for changes in number of partitions: %v", err)
		}
		select {
		case <-tv.closedCh:
			// If the TableViewImpl has been closed, stop checking for partition updates
			return
		case <-time.After(tv.options.AutoUpdatePartitionsInterval):
			continue
		}
	}
}

func (tv *TableViewImpl) Size() int {
	tv.dataMu.Lock()
	defer tv.dataMu.Unlock()
	return len(tv.data)
}

func (tv *TableViewImpl) IsEmpty() bool {
	tv.dataMu.Lock()
	defer tv.dataMu.Unlock()
	return tv.Size() == 0
}

func (tv *TableViewImpl) ContainsKey(key string) bool {
	tv.dataMu.Lock()
	defer tv.dataMu.Unlock()
	_, ok := tv.data[key]
	return ok
}

func (tv *TableViewImpl) Get(key string) interface{} {
	tv.dataMu.Lock()
	defer tv.dataMu.Unlock()
	return tv.data[key]
}

func (tv *TableViewImpl) Entries() map[string]interface{} {
	tv.dataMu.Lock()
	defer tv.dataMu.Unlock()
	data := make(map[string]interface{}, len(tv.data))
	for k, v := range tv.data {
		data[k] = v
	}
	return tv.data
}

func (tv *TableViewImpl) Keys() []string {
	tv.dataMu.Lock()
	defer tv.dataMu.Unlock()
	keys := make([]string, len(tv.data))
	i := 0
	for k := range tv.data {
		keys[i] = k
		i++
	}
	return keys
}

func (tv *TableViewImpl) ForEach(action func(string, interface{}) error) error {
	tv.dataMu.Lock()
	defer tv.dataMu.Unlock()
	for k, v := range tv.data {
		if err := action(k, v); err != nil {
			return err
		}
	}
	return nil
}

func (tv *TableViewImpl) ForEachAndListen(action func(string, interface{}) error) error {
	tv.listenersMu.Lock()
	defer tv.listenersMu.Unlock()

	if err := tv.ForEach(action); err != nil {
		return err
	}

	tv.listeners = append(tv.listeners, action)
	return nil
}

func (tv *TableViewImpl) Close() {
	tv.readersMu.Lock()
	defer tv.readersMu.Unlock()

	if !tv.closed {
		tv.closed = true
		for _, cancelReader := range tv.cancelReaders {
			cancelReader.reader.Close()
		}
		close(tv.closedCh)
	}
}

func (tv *TableViewImpl) handleMessage(msg Message) {
	tv.dataMu.Lock()
	defer tv.dataMu.Unlock()

	payload := reflect.New(tv.options.SchemaValueType)
	if len(msg.Payload()) == 0 {
		delete(tv.data, msg.Key())
	} else {
		if err := msg.GetSchemaValue(payload.Interface()); err != nil {
			tv.logger.Errorf("msg.GetSchemaValue() failed with %v; msg is %v", err, msg)
		}
		tv.data[msg.Key()] = reflect.Indirect(payload).Interface()
	}

	for _, listener := range tv.listeners {
		if err := listener(msg.Key(), reflect.Indirect(payload).Interface()); err != nil {
			tv.logger.Errorf("table view listener failed for %v: %v", msg, err)
		}
	}
}

func (tv *TableViewImpl) watchReaderForNewMessages(ctx context.Context, reader Reader) {
	for {
		msg, err := reader.Next(ctx)
		if err != nil {
			tv.logger.Errorf("read next message failed for %s: %v", reader.Topic(), err)
		}
		var e *Error
		if (errors.As(err, &e) && e.Result() == ConsumerClosed) || errors.Is(err, context.Canceled) {
			return
		}
		if msg != nil {
			tv.handleMessage(msg)
		}
	}
}
