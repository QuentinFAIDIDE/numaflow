/*
Copyright 2022 The Numaproj Authors.

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

// Package generator contains an implementation of an in-memory generator that generates
// payloads in json format.
package generator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	rand2 "math/rand"
	"time"

	"go.uber.org/zap"

	dfv1 "github.com/numaproj/numaflow/pkg/apis/numaflow/v1alpha1"
	"github.com/numaproj/numaflow/pkg/forwarder"
	"github.com/numaproj/numaflow/pkg/isb"
	"github.com/numaproj/numaflow/pkg/metrics"
	"github.com/numaproj/numaflow/pkg/shared/logging"
	sourceforward "github.com/numaproj/numaflow/pkg/sources/forward"
	applier2 "github.com/numaproj/numaflow/pkg/sources/forward/applier"
	"github.com/numaproj/numaflow/pkg/sources/sourcer"
	"github.com/numaproj/numaflow/pkg/watermark/fetch"
	"github.com/numaproj/numaflow/pkg/watermark/publish"
	"github.com/numaproj/numaflow/pkg/watermark/store"
	"github.com/numaproj/numaflow/pkg/watermark/wmb"
)

var log = logging.NewLogger()

type Data struct {
	Value uint64 `json:"value,omitempty"`
	// only to ensure a desired message size
	Padding []byte `json:"padding,omitempty"`
}

// payload generated by the generator function
// look at newReadMessage function
type payload struct {
	Data      Data
	Createdts int64
}

// record is payload with offset
// internal construct of this package
type record struct {
	data   []byte
	offset int64
	key    string
	ts     int64
}

var recordGenerator = func(size int32, value *uint64, createdTS int64) []byte {

	data := Data{}
	if value != nil {
		data.Value = *value
	} else {
		data.Value = uint64(createdTS)
	}
	size = size - 8
	if size > 0 {
		// padding to guarantee the size of the message
		b := make([]byte, size)
		_, err := rand.Read(b) // we do not care about failures here.
		if err != nil {
			log.Warn("error while generating random bytes", err)
		}
		data.Padding = b
	}

	r := payload{Data: data, Createdts: createdTS}
	marshalled, err := json.Marshal(r)
	if err != nil {
		log.Errorf("Error marshalling the record [%v]", r)
	}
	return marshalled
}

type memGen struct {
	srcChan        chan record                        // srcChan provides a go channel that supplies generated data
	rpu            int                                // rpu - records per time unit
	keyCount       int32                              // keyCount is the number of unique keys in the payload
	value          *uint64                            // value is the optional uint64 number that can be set in the payload
	msgSize        int32                              // msgSize is the size of each generated message
	timeunit       time.Duration                      // timeunit - ticker will fire once per timeunit
	genFn          func(int32, *uint64, int64) []byte // genFn function that generates a payload as a byte array
	vertexName     string                             // name is the name of the source vertex
	pipelineName   string                             // pipelineName is the name of the pipeline
	cancelFn       context.CancelFunc                 // cancelFn terminates the source will not generate any more records.
	forwarder      *sourceforward.DataForward         // forwarder to read from the source and write to the inter step buffer.
	lifecycleCtx   context.Context                    // lifecycleCtx context is used to control the lifecycle of this instance.
	readTimeout    time.Duration                      // read timeout for the reader
	vertexInstance *dfv1.VertexInstance               // vertex instance
	jitter         time.Duration
	logger         *zap.SugaredLogger
}

type Option func(*memGen) error

// WithLogger is used to return logger information
func WithLogger(l *zap.SugaredLogger) Option {
	return func(o *memGen) error {
		o.logger = l
		return nil
	}
}

func WithReadTimeout(timeout time.Duration) Option {
	return func(o *memGen) error {
		o.readTimeout = timeout
		return nil
	}
}

// NewMemGen function creates an instance of generator.
func NewMemGen(
	vertexInstance *dfv1.VertexInstance,
	writers map[string][]isb.BufferWriter,
	fsd forwarder.ToWhichStepDecider,
	transformerApplier applier2.SourceTransformApplier,
	fetchWM fetch.SourceFetcher,
	toVertexPublisherStores map[string]store.WatermarkStore,
	publishWMStores store.WatermarkStore,
	idleManager wmb.IdleManager,
	opts ...Option) (sourcer.Sourcer, error) {

	// minimal CRDs don't have defaults
	rpu := 5
	if vertexInstance.Vertex.Spec.Source.Generator.RPU != nil {
		rpu = int(*(vertexInstance.Vertex.Spec.Source.Generator.RPU))
	}
	msgSize := int32(8)
	if vertexInstance.Vertex.Spec.Source.Generator.MsgSize != nil {
		msgSize = *vertexInstance.Vertex.Spec.Source.Generator.MsgSize
	}
	timeunit := time.Second
	if vertexInstance.Vertex.Spec.Source.Generator.Duration != nil {
		timeunit = vertexInstance.Vertex.Spec.Source.Generator.Duration.Duration
	}
	keyCount := int32(1)
	if vertexInstance.Vertex.Spec.Source.Generator.KeyCount != nil {
		keyCount = *(vertexInstance.Vertex.Spec.Source.Generator.KeyCount)
	}
	var value *uint64
	if vertexInstance.Vertex.Spec.Source.Generator.Value != nil {
		value = vertexInstance.Vertex.Spec.Source.Generator.Value
	}

	var jitter time.Duration
	if vertexInstance.Vertex.Spec.Source.Generator.Jitter != nil {
		jitter = vertexInstance.Vertex.Spec.Source.Generator.Jitter.Duration
	}

	genSrc := &memGen{
		rpu:            rpu,
		keyCount:       keyCount,
		value:          value,
		msgSize:        msgSize,
		timeunit:       timeunit,
		vertexName:     vertexInstance.Vertex.Spec.Name,
		pipelineName:   vertexInstance.Vertex.Spec.PipelineName,
		genFn:          recordGenerator,
		vertexInstance: vertexInstance,
		srcChan:        make(chan record, rpu*int(keyCount)*5),
		readTimeout:    3 * time.Second, // default timeout
		jitter:         jitter,
	}

	for _, o := range opts {
		if err := o(genSrc); err != nil {
			return nil, err
		}
	}
	if genSrc.logger == nil {
		genSrc.logger = logging.NewLogger()
	}

	// this context is to be used internally for controlling the lifecycle of generator
	ctx, cancel := context.WithCancel(context.Background())

	genSrc.lifecycleCtx = ctx
	genSrc.cancelFn = cancel

	forwardOpts := []sourceforward.Option{sourceforward.WithLogger(genSrc.logger)}
	if x := vertexInstance.Vertex.Spec.Limits; x != nil {
		if x.ReadBatchSize != nil {
			forwardOpts = append(forwardOpts, sourceforward.WithReadBatchSize(int64(*x.ReadBatchSize)))
		}
		if x.RetryInterval != nil {
			forwardOpts = append(forwardOpts, sourceforward.WithRetryInterval(x.RetryInterval.Duration))
		}
	}

	// create a source watermark publisher
	sourceWmPublisher := publish.NewSourcePublish(ctx, genSrc.pipelineName, genSrc.vertexName, publishWMStores,
		publish.WithDelay(vertexInstance.Vertex.Spec.Watermark.GetMaxDelay()), publish.WithDefaultPartitionIdx(vertexInstance.Replica))

	// we pass in the context to sourceForwarder as well so that it can shut down when we cancelFn the context
	sourceForwarder, err := sourceforward.NewDataForward(vertexInstance, genSrc, writers, fsd, transformerApplier, fetchWM, sourceWmPublisher, toVertexPublisherStores, idleManager, forwardOpts...)
	if err != nil {
		return nil, err
	}
	genSrc.forwarder = sourceForwarder

	return genSrc, nil
}

// GetName returns the name of the source
func (mg *memGen) GetName() string {
	return mg.vertexName
}

// Partitions returns the partitions for the source.
func (mg *memGen) Partitions(context.Context) []int32 {
	return []int32{mg.vertexInstance.Replica}
}

func (mg *memGen) IsEmpty() bool {
	return len(mg.srcChan) == 0
}

func (mg *memGen) Read(_ context.Context, count int64) ([]*isb.ReadMessage, error) {
	msgs := make([]*isb.ReadMessage, 0, count)
	// timeout should not be re-triggered for every run of the for loop. it is for the entire Read() call.
	timeout := time.After(mg.readTimeout)
loop:
	for i := int64(0); i < count; i++ {
		// since the Read call is blocking, and runs in an infinite loop,
		// we implement Read With Wait semantics
		select {
		case r := <-mg.srcChan:
			tickgenSourceReadCount.With(map[string]string{metrics.LabelVertex: mg.vertexName, metrics.LabelPipeline: mg.pipelineName}).Inc()
			msgs = append(msgs, mg.newReadMessage(r.key, r.data, r.offset, r.ts))
		case <-timeout:
			mg.logger.Debugw("Timed out waiting for messages to read.", zap.Duration("waited", mg.readTimeout))
			break loop
		}
	}
	return msgs, nil
}

func (mg *memGen) Pending(_ context.Context) (int64, error) {
	return isb.PendingNotAvailable, nil
}

// Ack acknowledges an array of offset.
func (mg *memGen) Ack(_ context.Context, offsets []isb.Offset) []error {
	return make([]error, len(offsets))
}

func (mg *memGen) Close() error {
	return nil
}

func (mg *memGen) Stop() {
	mg.cancelFn()
	mg.forwarder.Stop()
}

func (mg *memGen) ForceStop() {
	mg.Stop()
	mg.forwarder.ForceStop()

}

// Start starts reading from the source
// context is used to control the lifecycle of this component.
// this context will be used to shut down the vertex once an os.signal is received.
func (mg *memGen) Start() <-chan struct{} {
	mg.generator(mg.lifecycleCtx, mg.rpu, mg.timeunit)
	return mg.forwarder.Start()
}

func (mg *memGen) NewWorker(ctx context.Context, rate int) func(chan time.Time, chan struct{}) {

	return func(tickChan chan time.Time, done chan struct{}) {
		defer func() {
			// empty any pending ticks
			if len(tickChan) > 0 {
				log.Info("emptying any pending ticks")
				for len(tickChan) > 0 {
					<-tickChan
				}
			}
		}()
		defer close(done)
		defer close(mg.srcChan)

		for {
			select {
			case <-ctx.Done():
				return
			case ts := <-tickChan:
				tickgenSourceCount.With(map[string]string{metrics.LabelVertex: mg.vertexName, metrics.LabelPipeline: mg.pipelineName})
				// we would generate all the keys in a round robin fashion
				// even if there are multiple pods, all the pods will generate same keys in the same order.
				// TODO: alternatively, we could also think about generating a subset of keys per pod.
				t := ts.UnixNano()
				for i := 0; i < rate; i++ {
					for k := int32(0); k < mg.keyCount; k++ {
						key := fmt.Sprintf("key-%d-%d", mg.vertexInstance.Replica, k)
						d := mg.genFn(mg.msgSize, mg.value, t)
						r := record{data: d, offset: time.Now().UTC().UnixNano(), key: key, ts: t}
						select {
						case <-ctx.Done():
							log.Info("Context.Done is called. returning from the inner function")
							return
						case mg.srcChan <- r:
						}
					}
				}
			}
		}
	}
}

// generator fires once per time unit and generates records and writes them to the channel
func (mg *memGen) generator(ctx context.Context, rate int, timeunit time.Duration) {
	go func() {
		// capping the rate to 10000 msgs/sec
		if rate > 10000 {
			log.Infow("Capping the rate to 10000 msg/sec. rate has been changed from %d to 10000", rate)
			rate = 10000
		}

		tickChan := make(chan time.Time, 1000)
		doneChan := make(chan struct{})
		childCtx, childCancel := context.WithCancel(ctx)

		defer childCancel()

		// make sure that there is only one worker all the time.
		// even when there is back pressure, max number of go routines inflight should be 1.
		// at the same time, we don't want to miss any ticks that cannot be processed.
		worker := mg.NewWorker(childCtx, rate)
		go worker(tickChan, doneChan)

		ticker := time.NewTicker(timeunit)
		defer ticker.Stop()
		for {
			select {
			// we don't need to wait for ticker to fire to return
			// when the context closes
			case <-ctx.Done():
				log.Info("Context.Done is called. exiting generator loop.")
				childCancel()
				<-doneChan
				return
			case ts := <-ticker.C:
				tickChan <- ts
			}
		}
	}()
}

func (mg *memGen) newReadMessage(key string, payload []byte, offset int64, et int64) *isb.ReadMessage {
	readOffset := isb.NewSimpleIntPartitionOffset(offset, mg.vertexInstance.Replica)
	msg := isb.Message{
		Header: isb.Header{
			// TODO: insert the right time based on the generator
			MessageInfo: isb.MessageInfo{EventTime: timeFromNanos(et, mg.jitter)},
			ID:          readOffset.String(),
			Keys:        []string{key},
		},
		Body: isb.Body{Payload: payload},
	}

	return &isb.ReadMessage{
		ReadOffset: readOffset,
		Message:    msg,
	}
}

func timeFromNanos(etime int64, jitter time.Duration) time.Time {
	// un-parseable json or invalid time format will be substituted with current time.
	if etime > 0 {
		updatedTs := time.Unix(0, etime)
		if jitter.Seconds() == 0 {
			return updatedTs
		}
		d := rand2.Intn(int(jitter.Seconds()))
		return updatedTs.Add(time.Duration(-d) * time.Second)
	}
	return time.Now()
}
