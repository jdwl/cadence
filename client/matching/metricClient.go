package matching

import (
	m "code.uber.internal/devexp/minions/.gen/go/matching"
	workflow "code.uber.internal/devexp/minions/.gen/go/shared"
	"code.uber.internal/devexp/minions/common/metrics"
)

var _ Client = (*metricClient)(nil)

type metricClient struct {
	client        Client
	metricsClient metrics.Client
}

// NewMetricClient creates a new instance of Client that emits metrics
func NewMetricClient(client Client, metricsClient metrics.Client) Client {
	return &metricClient{
		client:        client,
		metricsClient: metricsClient,
	}
}

func (c *metricClient) AddActivityTask(addRequest *m.AddActivityTaskRequest) error {
	c.metricsClient.IncCounter(metrics.MatchingClientAddActivityTaskScope, metrics.WorkflowRequests)

	sw := c.metricsClient.StartTimer(metrics.MatchingClientAddActivityTaskScope, metrics.WorkflowLatency)
	err := c.client.AddActivityTask(addRequest)
	sw.Stop()

	if err != nil {
		c.metricsClient.IncCounter(metrics.MatchingClientAddActivityTaskScope, metrics.WorkflowFailures)
	}

	return err
}

func (c *metricClient) AddDecisionTask(addRequest *m.AddDecisionTaskRequest) error {
	c.metricsClient.IncCounter(metrics.MatchingClientAddDecisionTaskScope, metrics.WorkflowRequests)

	sw := c.metricsClient.StartTimer(metrics.MatchingClientAddDecisionTaskScope, metrics.WorkflowLatency)
	err := c.client.AddDecisionTask(addRequest)
	sw.Stop()

	if err != nil {
		c.metricsClient.IncCounter(metrics.MatchingClientAddDecisionTaskScope, metrics.WorkflowFailures)
	}

	return err
}

func (c *metricClient) PollForActivityTask(pollRequest *workflow.PollForActivityTaskRequest) (*workflow.PollForActivityTaskResponse, error) {
	c.metricsClient.IncCounter(metrics.MatchingClientPollForActivityTaskScope, metrics.WorkflowRequests)

	sw := c.metricsClient.StartTimer(metrics.MatchingClientPollForActivityTaskScope, metrics.WorkflowLatency)
	resp, err := c.client.PollForActivityTask(pollRequest)
	sw.Stop()

	if err != nil {
		c.metricsClient.IncCounter(metrics.MatchingClientPollForActivityTaskScope, metrics.WorkflowFailures)
	}

	return resp, err
}

func (c *metricClient) PollForDecisionTask(pollRequest *workflow.PollForDecisionTaskRequest) (*workflow.PollForDecisionTaskResponse, error) {
	c.metricsClient.IncCounter(metrics.MatchingClientPollForDecisionTaskScope, metrics.WorkflowRequests)

	sw := c.metricsClient.StartTimer(metrics.MatchingClientPollForDecisionTaskScope, metrics.WorkflowLatency)
	resp, err := c.client.PollForDecisionTask(pollRequest)
	sw.Stop()

	if err != nil {
		c.metricsClient.IncCounter(metrics.MatchingClientPollForDecisionTaskScope, metrics.WorkflowFailures)
	}

	return resp, err
}