// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package metrics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/ptypes/duration"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"github.com/golang/protobuf/ptypes/timestamp"
	"google.golang.org/api/iterator"
	monitoringpb "google.golang.org/genproto/googleapis/monitoring/v3"

	"github.com/elastic/beats/v7/x-pack/metricbeat/module/gcp"
	"github.com/elastic/elastic-agent-libs/logp"
)

type metricsRequester struct {
	config config

	client *monitoring.MetricClient

	logger *logp.Logger
}

type timeSeriesWithAligner struct {
	timeSeries []*monitoringpb.TimeSeries
	aligner    string
}

func (r *metricsRequester) Metric(ctx context.Context, serviceName, metricType string, timeInterval *monitoringpb.TimeInterval, aligner string) timeSeriesWithAligner {
	timeSeries := make([]*monitoringpb.TimeSeries, 0)

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:     "projects/" + r.config.ProjectID,
		Interval: timeInterval,
		View:     monitoringpb.ListTimeSeriesRequest_FULL,
		Filter:   r.getFilterForMetric(serviceName, metricType),
		Aggregation: &monitoringpb.Aggregation{
			PerSeriesAligner: gcp.AlignersMapToGCP[aligner],
			AlignmentPeriod:  r.config.period,
		},
	}

	it := r.client.ListTimeSeries(ctx, req)

	for {
		resp, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}

		if err != nil {
			r.logger.Errorf("Could not read time series value: %s: %v", metricType, err)
			break
		}

		timeSeries = append(timeSeries, resp)
	}

	out := timeSeriesWithAligner{
		aligner:    aligner,
		timeSeries: timeSeries,
	}

	return out
}

func (r *metricsRequester) Metrics(ctx context.Context, serviceName string, aligner string, metricsToCollect map[string]metricMeta) ([]timeSeriesWithAligner, error) {
	var lock sync.Mutex
	var wg sync.WaitGroup
	results := make([]timeSeriesWithAligner, 0)

	for mt, meta := range metricsToCollect {
		wg.Add(1)

		metricMeta := meta
		go func(mt string) {
			defer wg.Done()

			r.logger.Debugf("For metricType %s, metricMeta = %d,  aligner = %s", mt, metricMeta, aligner)
			interval, aligner := getTimeIntervalAligner(metricMeta.ingestDelay, metricMeta.samplePeriod, r.config.period, aligner)
			ts := r.Metric(ctx, serviceName, mt, interval, aligner)
			lock.Lock()
			defer lock.Unlock()
			results = append(results, ts)
		}(mt)
	}

	wg.Wait()
	return results, nil
}

func (r *metricsRequester) buildRegionsFilter(regions []string, label string) string {
	if len(regions) == 0 {
		return ""
	}

	var filter strings.Builder

	// No. of regions added to the filter string.
	var regionsCount uint

	for _, region := range regions {
		// If 1 region has been added and the iteration continues, add the OR operator.
		if regionsCount > 0 {
			filter.WriteString("OR")
			filter.WriteString(" ")
		}

		filter.WriteString(fmt.Sprintf("%s = starts_with(\"%s\")", label, strings.TrimSuffix(region, "*")))
		filter.WriteString(" ")

		regionsCount++
	}

	switch {
	// If the filter string has more than 1 region, parentheses are added for better filter readability.
	case regionsCount > 1:
		return fmt.Sprintf("(%s)", strings.TrimSpace(filter.String()))
	default:
		return strings.TrimSpace(filter.String())
	}
}

// getFilterForMetric returns the filter associated with the corresponding filter. Some services like Pub/Sub fails
// if they have a region specified.
func (r *metricsRequester) getFilterForMetric(serviceName, m string) string {
	f := fmt.Sprintf(`metric.type="%s"`, m)
	if r.config.Zone == "" && r.config.Region == "" && len(r.config.Regions) == 0 {
		return f
	}

	switch serviceName {
	case gcp.ServiceCompute:
		if r.config.Region != "" && r.config.Zone != "" {
			r.logger.Warnf("when region %s and zone %s config parameter "+
				"both are provided, only use region", r.config.Regions, r.config.Zone)
		}

		if r.config.Region != "" && len(r.config.Regions) != 0 {
			r.logger.Warnf("when region %s and regions config parameters are both provided, use region", r.config.Region)
		}

		switch {
		case r.config.Region != "":
			f = fmt.Sprintf("%s AND %s = starts_with(\"%s\")", f, gcp.ComputeResourceLabel, strings.TrimSuffix(r.config.Region, "*"))
		case r.config.Zone != "":
			f = fmt.Sprintf("%s AND %s = starts_with(\"%s\")", f, gcp.ComputeResourceLabel, strings.TrimSuffix(r.config.Zone, "*"))
		case len(r.config.Regions) != 0:
			regionsFilter := r.buildRegionsFilter(r.config.Regions, gcp.ComputeResourceLabel)
			f = fmt.Sprintf("%s AND %s", f, regionsFilter)
		}
	case gcp.ServiceGKE:
		if r.config.Region != "" && r.config.Zone != "" {
			r.logger.Warnf("when region %s and zone %s config parameter "+
				"both are provided, only use region", r.config.Region, r.config.Zone)
		}

		switch {
		case r.config.Region != "":
			region := strings.TrimSuffix(r.config.Region, "*")
			f = fmt.Sprintf("%s AND resource.label.location=starts_with(\"%s\")", f, region)
		case r.config.Zone != "":
			zone := strings.TrimSuffix(r.config.Zone, "*")
			f = fmt.Sprintf("%s AND resource.label.location=starts_with(\"%s\")", f, zone)
		case len(r.config.Regions) != 0:
			regionsFilter := r.buildRegionsFilter(r.config.Regions, gcp.GKEResourceLabel)
			f = fmt.Sprintf("%s AND %s", f, regionsFilter)
		}
	case gcp.ServicePubsub, gcp.ServiceLoadBalancing, gcp.ServiceCloudFunctions, gcp.ServiceFirestore:
		return f
	case gcp.ServiceDataproc:
		if r.config.Region != "" && len(r.config.Regions) != 0 {
			r.logger.Warnf("when region %s and regions config parameters are both provided, use region", r.config.Region)
		}

		switch {
		case r.config.Region != "":
			f = fmt.Sprintf("%s AND %s = starts_with(\"%s\")", f, gcp.DataprocResourceLabel, strings.TrimSuffix(r.config.Region, "*"))
		case len(r.config.Regions) != 0:
			regionsFilter := r.buildRegionsFilter(r.config.Regions, gcp.DataprocResourceLabel)
			f = fmt.Sprintf("%s AND %s", f, regionsFilter)
		}
	case gcp.ServiceStorage:
		if r.config.Region != "" && len(r.config.Regions) != 0 {
			r.logger.Warnf("when region %s and regions config parameters are both provided, use region", r.config.Region)
		}

		switch {
		case r.config.Region != "":
			f = fmt.Sprintf(`%s AND resource.labels.location = "%s"`, f, r.config.Region)
		case len(r.config.Regions) != 0:
			regionsFilter := r.buildRegionsFilter(r.config.Regions, gcp.StorageResourceLabel)
			f = fmt.Sprintf("%s AND %s", f, regionsFilter)
		}
	case gcp.ServiceCloudSQL:
		if r.config.Region != "" && len(r.config.Regions) != 0 {
			r.logger.Warnf("when region %s and regions config parameters are both provided, use region", r.config.Region)
		}

		switch {
		case r.config.Region != "":
			region := strings.TrimSuffix(r.config.Region, "*")
			f = fmt.Sprintf("%s AND %s = starts_with(\"%s\")", f, gcp.CloudSQLResourceLabel, region)
		case len(r.config.Regions) != 0:
			regionsFilter := r.buildRegionsFilter(r.config.Regions, gcp.CloudSQLResourceLabel)
			f = fmt.Sprintf("%s AND %s", f, regionsFilter)
		}
	case gcp.ServiceRedis:
		if r.config.Region != "" && len(r.config.Regions) != 0 {
			r.logger.Warnf("when region %s and regions config parameters are both provided, use region", r.config.Region)
		}

		switch {
		case r.config.Region != "":
			region := strings.TrimSuffix(r.config.Region, "*")
			f = fmt.Sprintf("%s AND %s = starts_with(\"%s\")", f, gcp.RedisResourceLabel, region)
		case len(r.config.Regions) != 0:
			regionsFilter := r.buildRegionsFilter(r.config.Regions, gcp.RedisResourceLabel)
			f = fmt.Sprintf("%s AND %s", f, regionsFilter)
		}
	default:
		if r.config.Region != "" && r.config.Zone != "" {
			r.logger.Warnf("when region %s and zone %s config parameter "+
				"both are provided, only use region", r.config.Region, r.config.Zone)
		}

		switch {
		case r.config.Region != "":
			region := strings.TrimSuffix(r.config.Region, "*")
			f = fmt.Sprintf(`%s AND resource.labels.zone = starts_with("%s")`, f, region)
		case r.config.Zone != "":
			zone := strings.TrimSuffix(r.config.Zone, "*")
			f = fmt.Sprintf(`%s AND resource.labels.zone = starts_with("%s")`, f, zone)
		}
	}

	r.logger.Debugf("ListTimeSeries API filter = %s", f)

	return f
}

// Returns a GCP TimeInterval based on the ingestDelay and samplePeriod from ListMetricDescriptor
func getTimeIntervalAligner(ingestDelay time.Duration, samplePeriod time.Duration, collectionPeriod *duration.Duration, inputAligner string) (*monitoringpb.TimeInterval, string) {
	var startTime, endTime, currentTime time.Time
	var needsAggregation bool
	currentTime = time.Now().UTC()

	// When samplePeriod < collectionPeriod, aggregation will be done in ListTimeSeriesRequest.
	// For example, samplePeriod = 60s, collectionPeriod = 300s, if perSeriesAligner is not given,
	// ALIGN_MEAN will be used by default.
	if int64(samplePeriod.Seconds()) < collectionPeriod.Seconds {
		endTime = currentTime.Add(-ingestDelay)
		startTime = endTime.Add(-time.Duration(collectionPeriod.Seconds) * time.Second)
		needsAggregation = true
	}

	// When samplePeriod == collectionPeriod, aggregation is not needed
	// When samplePeriod > collectionPeriod, aggregation is not needed, use sample period
	// to determine startTime and endTime to make sure there will be data point in this time range.
	if int64(samplePeriod.Seconds()) >= collectionPeriod.Seconds {
		endTime = currentTime.Add(-ingestDelay)
		startTime = endTime.Add(-samplePeriod)
		needsAggregation = false
	}

	interval := &monitoringpb.TimeInterval{
		StartTime: &timestamp.Timestamp{
			Seconds: startTime.Unix(),
		},
		EndTime: &timestamp.Timestamp{
			Seconds: endTime.Unix(),
		},
	}

	// Default aligner for aggregation is ALIGN_NONE if it's not given
	updatedAligner := gcp.DefaultAligner
	if needsAggregation && inputAligner != "" {
		updatedAligner = inputAligner
	}

	return interval, updatedAligner
}
