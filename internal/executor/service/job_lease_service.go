package service

import (
	"context"
	"fmt"
	"github.com/G-Research/k8s-batch/internal/armada/api"
	"github.com/G-Research/k8s-batch/internal/common"
	"github.com/G-Research/k8s-batch/internal/executor/submitter"
	"github.com/G-Research/k8s-batch/internal/executor/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	listers "k8s.io/client-go/listers/core/v1"
	"strings"
	"time"
)

type JobLeaseService struct {
	PodLister      listers.PodLister
	NodeLister     listers.NodeLister
	JobSubmitter   submitter.JobSubmitter
	QueueClient    api.AggregatedQueueClient
	CleanupService PodCleanupService
	ClusterId      string
}

//TODO split into separate functions
func (jobLeaseService JobLeaseService) RequestJobLeasesAndFillSpareClusterCapacity() {
	allNodes, err := jobLeaseService.NodeLister.List(labels.Everything())
	if err != nil {
		fmt.Println("Error getting node information")
	}

	allPods, err := jobLeaseService.PodLister.List(labels.Everything())
	if err != nil {
		fmt.Println("Error getting pod information")
	}

	processingNodes := getAllAvailableProcessingNodes(allNodes)
	podsOnProcessingNodes := getAllPodsOnNodes(allPods, processingNodes)
	activePodsOnProcessingNodes := filterCompletedPods(podsOnProcessingNodes)

	totalNodeResource := calculateTotalResource(processingNodes)
	totalPodResource := calculateTotalResourceLimit(activePodsOnProcessingNodes)

	availableResource := totalNodeResource.DeepCopy()
	availableResource.Sub(totalPodResource)

	newJobs, err := jobLeaseService.requestJobs(availableResource)
	if err != nil {
		fmt.Printf("Failed to lease new jobs because %s \n", err)
	} else {
		for _, job := range newJobs {
			_, err = jobLeaseService.JobSubmitter.SubmitJob(job)
			if err != nil {
				fmt.Printf("Failed to submit job %s because %s \n", job.Id, err)
			}
		}
	}
}

func (jobLeaseService JobLeaseService) ManageJobLeases() {
	managedPodSelector, err := util.CreateLabelSelectorForManagedPods()
	if err != nil {
		//TODO Handle error case
	}

	allManagedPods, err := jobLeaseService.PodLister.List(managedPodSelector)
	if err != nil {
		//TODO Handle error case
	}

	podsToRenew, podsToCleanup := splitRunningAndFinishedPods(allManagedPods)

	jobLeaseService.renewJobLeases(podsToRenew)
	jobLeaseService.endJobLeases(podsToCleanup)
}

func splitRunningAndFinishedPods(pods []*v1.Pod) (running []*v1.Pod, finished []*v1.Pod) {
	runningPods := make([]*v1.Pod, 0)
	finishedPods := make([]*v1.Pod, 0)

	for _, pod := range pods {
		if IsPodReadyForCleanup(pod) {
			finishedPods = append(finishedPods, pod)
		} else {
			runningPods = append(runningPods, pod)
		}
	}

	return runningPods, finishedPods
}

func (jobLeaseService JobLeaseService) renewJobLeases(pods []*v1.Pod) {
	if len(pods) <= 0 {
		return
	}
	jobIds := util.ExtractJobIds(pods)
	fmt.Printf("Renewing lease for %s \n", strings.Join(jobIds, ","))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := jobLeaseService.QueueClient.RenewLease(ctx, &api.IdList{Ids: jobIds})

	if err != nil {
		fmt.Printf("Failed to renew lease for jobs because %s \n", err)
	}
}

func (jobLeaseService JobLeaseService) endJobLeases(pods []*v1.Pod) {
	if len(pods) <= 0 {
		return
	}
	jobIds := util.ExtractJobIds(pods)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := jobLeaseService.QueueClient.ReportDone(ctx, &api.IdList{Ids: jobIds})

	if err != nil {
		fmt.Printf("Failed cleaning up jobs because %s \n", err)
	}

	jobLeaseService.CleanupService.DeletePods(pods)
}

func (jobLeaseService JobLeaseService) requestJobs(availableResource common.ComputeResources) ([]*api.Job, error) {
	leaseRequest := api.LeaseRequest{
		ClusterID: jobLeaseService.ClusterId,
		Resources: availableResource,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := jobLeaseService.QueueClient.LeaseJobs(ctx, &leaseRequest)

	if err != nil {
		return make([]*api.Job, 0), err
	}

	return response.Job, nil
}

func getAllAvailableProcessingNodes(nodes []*v1.Node) []*v1.Node {
	processingNodes := make([]*v1.Node, 0, len(nodes))

	for _, node := range nodes {
		if isAvailableProcessingNode(node) {
			processingNodes = append(processingNodes, node)
		}
	}

	return processingNodes
}

func isAvailableProcessingNode(node *v1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}

	noSchedule := false

	for _, taint := range node.Spec.Taints {
		if taint.Effect == v1.TaintEffectNoSchedule {
			noSchedule = true
			break
		}
	}

	if noSchedule {
		return false
	}

	return true
}

func getAllPodsOnNodes(pods []*v1.Pod, nodes []*v1.Node) []*v1.Pod {
	podsBelongingToNodes := make([]*v1.Pod, 0, len(pods))

	nodeMap := make(map[string]*v1.Node)
	for _, node := range nodes {
		nodeMap[node.Name] = node
	}

	for _, pod := range pods {
		if _, present := nodeMap[pod.Spec.NodeName]; present {
			podsBelongingToNodes = append(podsBelongingToNodes, pod)
		}
	}

	return podsBelongingToNodes
}

func calculateTotalResource(nodes []*v1.Node) common.ComputeResources {
	totalResources := make(common.ComputeResources)
	for _, node := range nodes {
		nodeAllocatableResource := common.FromResourceList(node.Status.Allocatable)
		totalResources.Add(nodeAllocatableResource)
	}
	return totalResources
}

func calculateTotalResourceLimit(pods []*v1.Pod) common.ComputeResources {
	totalResources := make(common.ComputeResources)
	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			containerResourceLimit := common.FromResourceList(container.Resources.Limits)
			totalResources.Add(containerResourceLimit)
		}
		// Todo determine what to do about init containers? How does Kubernetes scheduler handle these
	}
	return totalResources
}

func filterCompletedPods(pods []*v1.Pod) []*v1.Pod {
	completedPods := make([]*v1.Pod, 0, len(pods))

	for _, pod := range pods {
		if util.IsInTerminalState(pod) {
			completedPods = append(completedPods, pod)
		}
	}

	return completedPods
}