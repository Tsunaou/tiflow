// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package mq

import (
	"context"
	"net/url"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/tiflow/cdc/sink/mq/dispatcher"
	"github.com/pingcap/tiflow/cdc/sink/mq/producer/kafka"
	"github.com/pingcap/tiflow/cdc/sinkv2/ddlsink/mq/ddlproducer"
	"github.com/pingcap/tiflow/cdc/sinkv2/util"
	"github.com/pingcap/tiflow/pkg/config"
	cerror "github.com/pingcap/tiflow/pkg/errors"
	pkafka "github.com/pingcap/tiflow/pkg/sink/kafka"
	"go.uber.org/zap"
)

// NewKafkaDDLSink will verify the config and create a Kafka DDL Sink.
func NewKafkaDDLSink(
	ctx context.Context,
	sinkURI *url.URL,
	replicaConfig *config.ReplicaConfig,
	adminClientCreator pkafka.ClusterAdminClientCreator,
	clientCreator pkafka.ClientCreator,
	producerCreator ddlproducer.Factory,
) (_ *ddlSink, err error) {
	topic, err := util.GetTopic(sinkURI)
	if err != nil {
		return nil, errors.Trace(err)
	}

	options := pkafka.NewOptions()
	if err := options.Apply(sinkURI); err != nil {
		return nil, cerror.WrapError(cerror.ErrKafkaInvalidConfig, err)
	}
	saramaConfig, err := pkafka.NewSaramaConfig(ctx, options)
	if err != nil {
		return nil, errors.Trace(err)
	}

	adminClient, err := adminClientCreator(options.BrokerEndpoints, saramaConfig)
	if err != nil {
		return nil, cerror.WrapError(cerror.ErrKafkaNewSaramaProducer, err)
	}
	// We must close adminClient when this func return cause by an error
	// otherwise the adminClient will never be closed and lead to a goroutine leak.
	defer func() {
		if err != nil {
			if closeErr := adminClient.Close(); closeErr != nil {
				log.Error("Close admin client failed in kafka "+
					"DDL sink", zap.Error(closeErr))
			}
		}
	}()

	if err := kafka.AdjustConfig(adminClient, options, saramaConfig, topic); err != nil {
		return nil, cerror.WrapError(cerror.ErrKafkaNewSaramaProducer, err)
	}

	protocol, err := util.GetProtocol(replicaConfig.Sink.Protocol)
	if err != nil {
		return nil, errors.Trace(err)
	}

	client, err := clientCreator(options.BrokerEndpoints, saramaConfig)
	if err != nil {
		return nil, cerror.WrapError(cerror.ErrKafkaNewSaramaProducer, err)
	}

	start := time.Now()
	log.Info("Try to create a DDL sink producer",
		zap.Any("options", options))
	p, err := producerCreator(ctx, client, adminClient)
	log.Info("DDL sink producer client created", zap.Duration("duration", time.Since(start)))
	if err != nil {
		return nil, cerror.WrapError(cerror.ErrKafkaNewSaramaProducer, err)
	}
	// Preventing leaks when error occurs.
	// This also closes the client in p.Close().
	defer func() {
		if err != nil {
			p.Close()
		}
	}()

	topicManager, err := util.GetTopicManagerAndTryCreateTopic(
		topic,
		options.DeriveTopicConfig(),
		client,
		adminClient,
	)
	if err != nil {
		return nil, errors.Trace(err)
	}

	eventRouter, err := dispatcher.NewEventRouter(replicaConfig, topic)
	if err != nil {
		return nil, errors.Trace(err)
	}

	encoderConfig, err := util.GetEncoderConfig(sinkURI, protocol, replicaConfig,
		saramaConfig.Producer.MaxMessageBytes)
	if err != nil {
		return nil, errors.Trace(err)
	}

	s, err := newDDLSink(ctx, p, topicManager, eventRouter, encoderConfig)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return s, nil
}
