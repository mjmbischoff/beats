// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

//go:build integration

package logstash

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/common/fmtstr"
	"github.com/elastic/beats/v7/libbeat/esleg/eslegclient"
	"github.com/elastic/beats/v7/libbeat/idxmgmt"
	"github.com/elastic/beats/v7/libbeat/outputs"
	_ "github.com/elastic/beats/v7/libbeat/outputs/elasticsearch"
	"github.com/elastic/beats/v7/libbeat/outputs/outest"
	"github.com/elastic/beats/v7/libbeat/outputs/outil"
	"github.com/elastic/beats/v7/libbeat/publisher"
	"github.com/elastic/beats/v7/libbeat/publisher/queue"
	conf "github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp/logptest"
	"github.com/elastic/elastic-agent-libs/mapstr"
	"github.com/elastic/elastic-agent-libs/transport/httpcommon"
)

const (
	logstashTestDefaultTLSPort = "5055"

	elasticsearchDefaultHost = "localhost"
	elasticsearchDefaultPort = "9200"

	integrationTestWindowSize = 32
)

type esConnection struct {
	*eslegclient.Connection
	t     *testing.T
	index string
}

type testOutputer struct {
	outputs.NetworkClient
	*esConnection
	encoder queue.Encoder
}

type esSource interface {
	RefreshIndex()
}

type esCountReader interface {
	esSource
	Count() (int, error)
}

func getLogstashTLSHost() string {
	return fmt.Sprintf("%v:%v",
		getenv("LS_HOST", logstashDefaultHost),
		getenv("LS_LS_PORT", logstashTestDefaultTLSPort),
	)
}

func getElasticsearchHost() string {
	return fmt.Sprintf("http://%v:%v",
		getenv("ES_HOST", elasticsearchDefaultHost),
		getenv("ES_PORT", elasticsearchDefaultPort),
	)
}

func esConnect(t *testing.T, index string) *esConnection {
	ts := time.Now().UTC()

	host := getElasticsearchHost()
	indexFmt := fmtstr.MustCompileEvent(fmt.Sprintf("%s-%%{+yyyy.MM.dd}", index))
	indexFmtExpr, _ := outil.FmtSelectorExpr(indexFmt, "", outil.SelectorLowerCase)
	indexSel := outil.MakeSelector(indexFmtExpr)
	index, _ = indexSel.Select(&beat.Event{
		Timestamp: ts,
	})

	username := os.Getenv("ES_USER")
	password := os.Getenv("ES_PASS")
	transport := httpcommon.DefaultHTTPTransportSettings()
	transport.Timeout = 60 * time.Second
	client, err := eslegclient.NewConnection(eslegclient.ConnectionSettings{
		URL:       host,
		Username:  username,
		Password:  password,
		Transport: transport,
	}, logptest.NewTestingLogger(t, ""))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("cannot connect to LS: %s:", err)
	}
	if err != nil {
		t.Fatal(err)
	}

	// try to drop old index if left over from failed test
	_, _, _ = client.Delete(index, "", "", nil) // ignore error

	_, _, err = client.CreateIndex(index, mapstr.M{
		"settings": mapstr.M{
			"number_of_shards":   1,
			"number_of_replicas": 0,
		},
	})
	if err != nil {
		t.Fatalf("failed to create test index: %s", err)
	}

	es := &esConnection{}
	es.t = t
	es.Connection = client
	es.index = index
	return es
}

func testElasticsearchIndex(test string) string {
	return fmt.Sprintf("beat-es-int-%v-%d", test, os.Getpid())
}

func newTestLogstashOutput(t *testing.T, test string, tls bool) *testOutputer {
	windowSize := integrationTestWindowSize

	config := map[string]interface{}{
		"hosts":         []string{getLogstashHost()},
		"index":         testLogstashIndex(test),
		"bulk_max_size": &windowSize,
	}
	if tls {
		config["hosts"] = []string{getLogstashTLSHost()}
		// Disable hostname verification as we are writing to localhost.
		config["ssl.verification_mode"] = "certificate"
		config["ssl.certificate_authorities"] = []string{
			"../../../testing/environments/docker/logstash/pki/tls/certs/logstash.crt",
		}
	}

	output := newTestLumberjackOutput(t, test, config)
	index := testLogstashIndex(test)
	connection := esConnect(t, index)

	return &testOutputer{output, connection, nil}
}

func newTestElasticsearchOutput(t *testing.T, test string) *testOutputer {
	plugin := outputs.FindFactory("elasticsearch")
	if plugin == nil {
		t.Fatalf("No elasticsearch output plugin found")
	}

	index := testElasticsearchIndex(test)
	connection := esConnect(t, index)

	bulkSize := 0
	config, _ := conf.NewConfigFrom(map[string]interface{}{
		"hosts":            []string{getElasticsearchHost()},
		"index":            connection.index,
		"bulk_max_size":    &bulkSize,
		"username":         os.Getenv("ES_USER"),
		"password":         os.Getenv("ES_PASS"),
		"template.enabled": false,
	})

	logger := logptest.NewTestingLogger(t, "")
	info := beat.Info{Beat: "libbeat", Logger: logger}
	im, err := idxmgmt.DefaultSupport(info, conf.MustNewConfigFrom(
		map[string]interface{}{
			"setup.ilm.enabled": false,
		},
	))
	if err != nil {
		t.Fatal("init index management:", err)
	}

	grp, err := plugin(im, info, outputs.NewNilObserver(), config)
	if err != nil {
		t.Fatalf("init elasticsearch output plugin failed: %v", err)
	}

	es := &testOutputer{}
	es.NetworkClient = grp.Clients[0].(outputs.NetworkClient) //nolint:errcheck //safe to ignore in tests
	es.esConnection = connection
	// The Elasticsearch output requires events to be encoded
	// before calling Publish, so create an event encoder.
	es.encoder = grp.EncoderFactory()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	err = es.Connect(ctx)
	require.NoError(t, err)
	return es
}

func (es *esConnection) Cleanup() {
	_, _, err := es.Delete(es.index, "", "", nil)
	if err != nil {
		es.t.Errorf("Failed to delete index: %s", err)
	}
}

func (es *esConnection) Read() ([]map[string]interface{}, error) {
	_, _, err := es.Refresh(es.index)
	if err != nil {
		es.t.Errorf("Failed to refresh: %s", err)
	}

	params := map[string]string{}
	_, resp, err := es.SearchURI(es.index, "", params)
	if err != nil {
		es.t.Errorf("Failed to query elasticsearch for index(%s): %s", es.index, err)
		return nil, err
	}

	hits := make([]map[string]interface{}, len(resp.Hits.Hits))
	for i, hit := range resp.Hits.Hits {
		json.Unmarshal(hit, &hits[i]) //nolint:errcheck //This is a test file, can ignore
	}

	return hits, err
}

func (es *esConnection) RefreshIndex() {
	_, _, err := es.Refresh(es.index)
	if err != nil {
		es.t.Errorf("Failed to refresh: %s", err)
	}
}

func (es *esConnection) Count() (int, error) {
	_, _, err := es.Refresh(es.index)
	if err != nil {
		es.t.Errorf("Failed to refresh: %s", err)
	}

	params := map[string]string{}
	_, resp, err := es.CountSearchURI(es.index, "", params)
	if err != nil {
		es.t.Errorf("Failed to query elasticsearch for index(%s): %s", es.index, err)
		return 0, err
	}

	return resp.Count, nil
}

func waitUntilTrue(duration time.Duration, fn func() bool) bool {
	end := time.Now().Add(duration)
	for time.Now().Before(end) {
		if fn() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func checkIndex(reader esCountReader, minValues int) func() bool {
	return func() bool {
		reader.RefreshIndex()
		resp, err := reader.Count()
		return err != nil || resp >= minValues
	}
}

func TestSendMessageViaLogstashTCP(t *testing.T) {
	testSendMessageViaLogstash(t, "basic-tcp", false)
}

func TestSendMessageViaLogstashTLS(t *testing.T) {
	testSendMessageViaLogstash(t, "basic-tls", true)
}

func testSendMessageViaLogstash(t *testing.T, name string, tls bool) {

	ls := newTestLogstashOutput(t, name, tls)
	defer ls.Cleanup()

	batch := outest.NewBatch(
		beat.Event{
			Timestamp: time.Now(),
			Fields: mapstr.M{
				"host":    "test-host",
				"message": "hello world",
			},
		},
	)
	err := ls.Publish(context.Background(), batch)
	require.NoError(t, err)

	// wait for logstash event flush + elasticsearch
	waitUntilTrue(5*time.Second, checkIndex(ls, 1))

	// search value in logstash elasticsearch index
	resp, err := ls.Read()
	if err != nil {
		return
	}
	if len(resp) != 1 {
		t.Errorf("wrong number of results: %d", len(resp))
	}
}

func TestSendMultipleViaLogstashTCP(t *testing.T) {
	testSendMultipleViaLogstash(t, "multiple-tcp", false)
}

func TestSendMultipleViaLogstashTLS(t *testing.T) {
	testSendMultipleViaLogstash(t, "multiple-tls", true)
}

func testSendMultipleViaLogstash(t *testing.T, name string, tls bool) {
	ls := newTestLogstashOutput(t, name, tls)
	defer ls.Cleanup()
	for i := 0; i < 10; i++ {
		event := beat.Event{
			Timestamp: time.Now(),
			Fields: mapstr.M{
				"host":    "test-host",
				"type":    "log",
				"message": fmt.Sprintf("hello world - %v", i),
			},
		}
		ls.PublishEvent(event)
	}

	// wait for logstash event flush + elasticsearch
	waitUntilTrue(5*time.Second, checkIndex(ls, 10))

	// search value in logstash elasticsearch index
	resp, err := ls.Read()
	if err != nil {
		return
	}
	if len(resp) != 10 {
		t.Errorf("wrong number of results: %d", len(resp))
	}
}

func TestSendMultipleBigBatchesViaLogstashTCP(t *testing.T) {
	testSendMultipleBigBatchesViaLogstash(t, "multiple-big-tcp", false)
}

func TestSendMultipleBigBatchesViaLogstashTLS(t *testing.T) {
	testSendMultipleBigBatchesViaLogstash(t, "multiple-big-tls", true)
}

func testSendMultipleBigBatchesViaLogstash(t *testing.T, name string, tls bool) {
	testSendMultipleBatchesViaLogstash(t, name, 15, 4*integrationTestWindowSize, tls)
}

func TestSendMultipleSmallBatchesViaLogstashTCP(t *testing.T) {
	testSendMultipleSmallBatchesViaLogstash(t, "multiple-small-tcp", false)
}

func TestSendMultipleSmallBatchesViaLogstashTLS(t *testing.T) {
	testSendMultipleSmallBatchesViaLogstash(t, "multiple-small-tls", true)
}

func testSendMultipleSmallBatchesViaLogstash(t *testing.T, name string, tls bool) {
	testSendMultipleBatchesViaLogstash(t, name, 15, integrationTestWindowSize/2, tls)
}

func testSendMultipleBatchesViaLogstash(
	t *testing.T,
	name string,
	numBatches int,
	batchSize int,
	tls bool,
) {

	ls := newTestLogstashOutput(t, name, tls)
	defer ls.Cleanup()

	batches := make([][]beat.Event, 0, numBatches)
	for i := 0; i < numBatches; i++ {
		batch := make([]beat.Event, 0, batchSize)
		for j := 0; j < batchSize; j++ {
			event := beat.Event{
				Timestamp: time.Now(),
				Fields: mapstr.M{
					"host":    "test-host",
					"type":    "log",
					"message": fmt.Sprintf("batch hello world - %v", i*batchSize+j),
				},
			}
			batch = append(batch, event)
		}
		batches = append(batches, batch)
	}

	for _, batch := range batches {
		ok := ls.BulkPublish(batch)
		assert.Equal(t, true, ok)
	}

	// wait for logstash event flush + elasticsearch
	ok := waitUntilTrue(5*time.Second, checkIndex(ls, numBatches*batchSize))
	assert.True(t, ok) // check number of events matches total number of events

	// search value in logstash elasticsearch index
	resp, err := ls.Read()
	if err != nil {
		return
	}
	if len(resp) != 10 {
		t.Errorf("wrong number of results: %d", len(resp))
	}
}

func TestLogstashElasticOutputPluginCompatibleMessageTCP(t *testing.T) {
	testLogstashElasticOutputPluginCompatibleMessage(t, "cmp-tcp", false)
}

func TestLogstashElasticOutputPluginCompatibleMessageTLS(t *testing.T) {
	testLogstashElasticOutputPluginCompatibleMessage(t, "cmp-tls", true)
}

func testLogstashElasticOutputPluginCompatibleMessage(t *testing.T, name string, tls bool) {
	timeout := 10 * time.Second

	ls := newTestLogstashOutput(t, name, tls)
	defer ls.Cleanup()

	es := newTestElasticsearchOutput(t, name)
	defer es.Cleanup()

	ts := time.Now()
	event := beat.Event{
		Timestamp: ts,
		Fields: mapstr.M{
			"host":    "test-host",
			"type":    "log",
			"message": "hello world",
		},
	}

	es.PublishEvent(event)
	ls.PublishEvent(event)

	waitUntilTrue(timeout, checkIndex(es, 1))
	waitUntilTrue(timeout, checkIndex(ls, 1))

	// search value in logstash elasticsearch index
	lsResp, err := ls.Read()
	if err != nil {
		return
	}
	esResp, err := es.Read()
	if err != nil {
		return
	}

	// validate
	assert.Equal(t, len(lsResp), len(esResp))
	if len(lsResp) != 1 {
		t.Fatalf("wrong number of results: %d", len(lsResp))
	}

	checkEvent(t, lsResp[0], esResp[0])
}

func TestLogstashElasticOutputPluginBulkCompatibleMessageTCP(t *testing.T) {
	testLogstashElasticOutputPluginBulkCompatibleMessage(t, "cmpblk-tcp", false)
}

func TestLogstashElasticOutputPluginBulkCompatibleMessageTLS(t *testing.T) {
	testLogstashElasticOutputPluginBulkCompatibleMessage(t, "cmpblk-tls", true)
}

func testLogstashElasticOutputPluginBulkCompatibleMessage(t *testing.T, name string, tls bool) {

	timeout := 10 * time.Second

	ls := newTestLogstashOutput(t, name, tls)
	defer ls.Cleanup()

	es := newTestElasticsearchOutput(t, name)
	defer es.Cleanup()

	ts := time.Now()
	events := []beat.Event{
		{
			Timestamp: ts,
			Fields: mapstr.M{
				"host":    "test-host",
				"type":    "log",
				"message": "hello world",
			},
		},
	}

	ls.BulkPublish(events)
	es.BulkPublish(events)

	waitUntilTrue(timeout, checkIndex(ls, 1))
	waitUntilTrue(timeout, checkIndex(es, 1))

	// search value in logstash elasticsearch index
	lsResp, err := ls.Read()
	if err != nil {
		return
	}
	esResp, err := es.Read()
	if err != nil {
		return
	}

	// validate
	if len(lsResp) != len(esResp) {
		assert.Equal(t, len(lsResp), len(esResp))
		t.Fatalf("wrong number of results: es=%d, ls=%d",
			len(esResp), len(lsResp))
	}

	checkEvent(t, lsResp[0], esResp[0])
}

func checkEvent(t *testing.T, ls, es map[string]interface{}) {
	lsEvent, ok := ls["_source"].(map[string]interface{})
	assert.True(t, ok)
	esEvent, ok := es["_source"].(map[string]interface{})
	assert.True(t, ok)
	commonFields := []string{"@timestamp", "host", "type", "message"}
	for _, field := range commonFields {
		assert.NotNil(t, lsEvent[field])
		assert.NotNil(t, esEvent[field])
		assert.Equal(t, lsEvent[field], esEvent[field])
	}
}

func (t *testOutputer) PublishEvent(event beat.Event) {
	batch := encodeBatch[*outest.Batch](t.encoder, outest.NewBatch(event))
	t.Publish(context.Background(), batch) //nolint:errcheck //This is a test file
}

func (t *testOutputer) BulkPublish(events []beat.Event) bool {
	ok := false
	batch := encodeBatch[*outest.Batch](t.encoder, outest.NewBatch(events...))

	var wg sync.WaitGroup
	wg.Add(1)
	batch.OnSignal = func(sig outest.BatchSignal) {
		ok = sig.Tag == outest.BatchACK
		wg.Done()
	}

	t.Publish(context.Background(), batch) //nolint:errcheck //This is a test file
	wg.Wait()
	return ok
}

// encodeBatch encodes a publisher.Batch so it can be provided to
// Client.Publish and other helpers.
// This modifies the batch in place, but also returns its input batch
// to allow for easy chaining while creating test batches.
func encodeBatch[B publisher.Batch](encoder queue.Encoder, batch B) B {
	if encoder != nil {
		encodeEvents(encoder, batch.Events())
	}
	return batch
}

func encodeEvents(encoder queue.Encoder, events []publisher.Event) []publisher.Event {
	for i := range events {
		// Skip encoding if there's already encoded data present
		if events[i].EncodedEvent == nil {
			encoded, _ := encoder.EncodeEntry(events[i])
			event := encoded.(publisher.Event) //nolint:errcheck //This is a test file, can ignore
			events[i] = event
		}
	}
	return events
}
