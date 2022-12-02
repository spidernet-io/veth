package macvlan_overlay_one_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spidernet-io/cni-plugins/test/e2e/common"
)

var _ = Describe("MacvlanOverlayOne", Label("overlay", "one-nic"), func() {

	It("spiderdoctor connectivity should be succeed", Label("spiderdoctor"), func() {
		// create task spiderdoctor crd
		task.Name = name
		// schedule
		plan.StartAfterMinute = 0
		plan.RoundNumber = 2
		plan.IntervalMinute = 2
		plan.TimeoutMinute = 2
		task.Spec.Schedule = plan
		// target
		targetAgent.TestIngress = true
		targetAgent.TestEndpoint = true
		targetAgent.TestClusterIp = true
		targetAgent.TestMultusInterface = true
		targetAgent.TestNodePort = true
		targetAgent.TestIPv4 = &common.IPV4
		// TODO https://github.com/projectcalico/calico/issues/6877
		targetAgent.TestIPv6 = &testIPv6

		target.TargetAgent = targetAgent
		task.Spec.Target = target
		// request
		request.DurationInSecond = 5
		request.QPS = 1
		request.PerRequestTimeoutInMS = 15000

		task.Spec.Request = request
		// success condition

		condition.SuccessRate = &successRate
		condition.MeanAccessDelayInMs = &delayMs

		task.Spec.SuccessCondition = condition
		GinkgoWriter.Printf("spiderdoctor task: %+v \n", task)
		err := frame.CreateResource(task)
		Expect(err).NotTo(HaveOccurred(), " spiderdoctor nethttp crd create failed")

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*60*5)
		defer cancel()
		err = common.WaitSpiderdoctorTaskFinish(frame, task, ctx)
		Expect(err).NotTo(HaveOccurred(), "spiderdoctor task failed")
	})

	It("50 pod start and stop should be succeed", Label("concurrent"), func() {
		deployment := common.GenerateDeploymentYaml(deploymentName, namespace, label, annotations, 5)
		// start pod
		_, err := frame.CreateDeploymentUntilReady(deployment, 20*common.CtxTimeout)
		Expect(err).NotTo(HaveOccurred())
		// stop pod
		err = frame.RestartDeploymentPodUntilReady(deploymentName, namespace, 20*common.CtxTimeout)
		Expect(err).NotTo(HaveOccurred())
	})
})
