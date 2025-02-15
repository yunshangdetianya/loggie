/*
Copyright 2021 Loggie Authors

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

package elasticsearch

import (
	"context"
	"encoding/json"
	"fmt"
	eventer "github.com/loggie-io/loggie/pkg/core/event"
	"github.com/loggie-io/loggie/pkg/core/log"
	"github.com/loggie-io/loggie/pkg/util/pattern"
	"strings"

	"github.com/loggie-io/loggie/pkg/core/api"
	"github.com/loggie-io/loggie/pkg/sink/codec"
	"github.com/loggie-io/loggie/pkg/util/runtime"
	es "github.com/olivere/elastic/v7"
	"github.com/pkg/errors"
)

type ClientSet struct {
	Version             string
	config              *Config
	cli                 *es.Client
	codec               codec.Codec
	indexPattern        *pattern.Pattern
	documentIdPattern   *pattern.Pattern
	defaultIndexPattern *pattern.Pattern
}

type Client interface {
	BulkCreate(content []byte, index string) error
	Stop()
}

func NewClient(config *Config, cod codec.Codec, indexPattern *pattern.Pattern, documentIdPattern *pattern.Pattern,
	defaultIndexPattern *pattern.Pattern) (*ClientSet, error) {
	for i, h := range config.Hosts {
		if !strings.HasPrefix(h, "http") && !strings.HasPrefix(h, "https") {
			config.Hosts[i] = fmt.Sprintf("http://%s", h)
		}
	}
	var opts []es.ClientOptionFunc
	opts = append(opts, es.SetURL(config.Hosts...))
	if config.Sniff != nil {
		opts = append(opts, es.SetSniff(*config.Sniff))
	} else {
		// disable sniff by default
		opts = append(opts, es.SetSniff(false))
	}
	if config.Password != "" && config.UserName != "" {
		opts = append(opts, es.SetBasicAuth(config.UserName, config.Password))
	}
	if config.Schema != "" {
		opts = append(opts, es.SetScheme(config.Schema))
	}
	if config.Gzip != nil {
		opts = append(opts, es.SetGzip(*config.Gzip))
	}

	cli, err := es.NewClient(opts...)
	if err != nil {
		return nil, err
	}

	return &ClientSet{
		cli:                 cli,
		config:              config,
		codec:               cod,
		indexPattern:        indexPattern,
		documentIdPattern:   documentIdPattern,
		defaultIndexPattern: defaultIndexPattern,
	}, nil
}

func (c *ClientSet) BulkIndex(ctx context.Context, batch api.Batch) error {
	req := c.cli.Bulk()
	for _, event := range batch.Events() {
		headerObj := runtime.NewObject(event.Header())

		// select index
		idx, err := c.indexPattern.WithObject(headerObj).RenderWithStrict()
		if err != nil {
			failedConfig := c.config.IfRenderIndexFailed
			if !failedConfig.IgnoreError {
				log.Error("render elasticsearch index error: %v; event is: %s", err, event.String())
			}

			if failedConfig.DefaultIndex != "" { // if we had a default index, send events to this one
				defaultIdx, defaultIdxErr := c.defaultIndexPattern.WithObject(headerObj).Render()
				if defaultIdxErr != nil {
					log.Error("render default index error: %v", defaultIdxErr)
					continue
				}
				idx = defaultIdx
			} else if failedConfig.DropEvent {
				// ignore(drop) this event in default
				continue
			} else {
				return errors.WithMessage(err, "render elasticsearch index error")
			}
		}

		data, err := c.codec.Encode(event)
		if err != nil {
			return errors.WithMessagef(err, "codec encode event: %s error", event.String())
		}

		bulkIndexRequest := es.NewBulkIndexRequest().Index(idx).Doc(json.RawMessage(data))
		if c.config.Etype != "" {
			bulkIndexRequest.Type(c.config.Etype)
		}
		if c.config.OpType != "" {
			bulkIndexRequest.OpType(c.config.OpType)
		}
		if c.config.DocumentId != "" {
			id, err := c.documentIdPattern.WithObject(headerObj).Render()
			if err != nil {
				return errors.WithMessagef(err, "format documentId %s failed", c.config.DocumentId)
			}
			bulkIndexRequest.Id(id)
		}

		req.Add(bulkIndexRequest)
	}

	if req.NumberOfActions() == 0 {
		return errors.WithMessagef(eventer.ErrorDropEvent, "request to elasticsearch bulk is null")
	}

	ret, err := req.Do(ctx)
	if err != nil {
		return err
	}
	if ret.Errors {
		out, _ := json.Marshal(ret)
		return errors.Errorf("request to elasticsearch response error: %s", out)
	}

	return nil
}

func (c *ClientSet) Stop() {
	if c.cli != nil {
		c.cli.Stop()
	}
}
