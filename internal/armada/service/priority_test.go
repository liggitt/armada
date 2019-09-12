package service

import (
	"github.com/G-Research/k8s-batch/internal/armada/api"
	"github.com/stretchr/testify/assert"
	"testing"
	"time"
)

func TestPriorityService_GetQueuePriorities(t *testing.T) {

	q1 := &api.Queue{Name: "queue1", PriorityFactor: 2}
	q2 := &api.Queue{Name: "queue2", PriorityFactor: 1}
	q3 := &api.Queue{Name: "queue3", PriorityFactor: 1}
	q4 := &api.Queue{Name: "queue4", PriorityFactor: 1}
	q5 := &api.Queue{Name: "queue5", PriorityFactor: 0.5}

	service := NewMultiClusterPriorityService(
		mockUsageRepository{
			clusterUsageReports: map[string]*api.ClusterUsageReport{
				"cluster1": {ClusterId: "cluster1", ReportTime: time.Now()},
				"cluster2": {ClusterId: "cluster2", ReportTime: time.Now()},
			},
			clusterPriorities: map[string]map[string]float64{
				"cluster1": {
					"queue1": 1.5,
					"queue2": 1.5,
				},
				"cluster2": {
					"queue1": 1,
					"queue3": 1,
					"queue6": 1, // missing
				},
			},
		},
		mockQueueRepository{
			queues: []*api.Queue{q1, q2, q3, q4, q5},
		},
		mockMetricRecorder{})

	priorities, e := service.GetQueuePriorities()
	assert.Nil(t, e)
	assert.Equal(t, map[*api.Queue]float64{
		q1: 5,
		q2: 1.5,
		q3: 1,
		q4: minPriority,
		q5: minPriority * 0.5,
	}, priorities)
}

type mockQueueRepository struct {
	queues []*api.Queue
}

func (m mockQueueRepository) GetQueues() ([]*api.Queue, error) {
	return m.queues, nil
}

func (mockQueueRepository) CreateQueue(queue *api.Queue) error {
	panic("implement me")
}

type mockUsageRepository struct {
	clusterPriorities   map[string]map[string]float64
	clusterUsageReports map[string]*api.ClusterUsageReport
}

func (m mockUsageRepository) GetClusterUsageReports() (map[string]*api.ClusterUsageReport, error) {
	return m.clusterUsageReports, nil
}

func (mockUsageRepository) GetClusterPriority(clusterId string) (map[string]float64, error) {
	panic("implement me")
}

func (m mockUsageRepository) GetClusterPriorities(clusterIds []string) (map[string]map[string]float64, error) {
	return m.clusterPriorities, nil
}

func (mockUsageRepository) UpdateCluster(report *api.ClusterUsageReport, priorities map[string]float64) error {
	panic("implement me")
}

type mockMetricRecorder struct{}

func (mockMetricRecorder) RecordQueuePriorities(priorities map[*api.Queue]float64) {}