/*
Copyright 2020 The Knative Authors

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

package resources_test

import (
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	naming "knative.dev/eventing-rabbitmq/pkg/rabbitmqnaming"
	"knative.dev/eventing-rabbitmq/pkg/reconciler/trigger/resources"
	rabbitv1beta1 "knative.dev/eventing-rabbitmq/third_party/pkg/apis/rabbitmq.com/v1beta1"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
)

const namespace = "foobar"
const triggerName = "my-trigger"

func TestNewQueue(t *testing.T) {
	for _, tt := range []struct {
		name    string
		broker  *eventingv1.Broker
		trigger *eventingv1.Trigger
		want    *rabbitv1beta1.Queue
		wantErr string
	}{{
		name:   "Broker binding",
		broker: createBroker(),
		want: &rabbitv1beta1.Queue{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "b.foobar.testbroker.dlq.broker-test-uid",
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind:       "Broker",
						APIVersion: "eventing.knative.dev/v1",
						Name:       brokerName,
						UID:        brokerUID,
					},
				},
				Labels: map[string]string{"eventing.knative.dev/broker": "testbroker"},
			},
			Spec: rabbitv1beta1.QueueSpec{
				Name:       "b.foobar.testbroker.dlq.broker-test-uid",
				Durable:    true,
				AutoDelete: false,
				RabbitmqClusterReference: rabbitv1beta1.RabbitmqClusterReference{
					Name: rabbitmqcluster,
				},
			},
		},
	}, {
		name:    "Trigger binding, no filter",
		broker:  createBroker(),
		trigger: createTrigger(),
		want: &rabbitv1beta1.Queue{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "t.foobar.my-trigger.trigger-test-uid",
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind:       "Trigger",
						APIVersion: "eventing.knative.dev/v1",
						Name:       triggerName,
						UID:        triggerUID,
					},
				},
				Labels: map[string]string{
					"eventing.knative.dev/broker":  "testbroker",
					"eventing.knative.dev/trigger": "my-trigger",
				},
			},
			Spec: rabbitv1beta1.QueueSpec{
				Name:       "t.foobar.my-trigger.trigger-test-uid",
				Durable:    true,
				AutoDelete: false,
				RabbitmqClusterReference: rabbitv1beta1.RabbitmqClusterReference{
					Name: rabbitmqcluster,
				},
				Arguments: getTriggerQueueArguments(),
			},
		},
	}, {
		name:    "Trigger binding, filter",
		broker:  createBroker(),
		trigger: createTriggerWithFilter(),
		want: &rabbitv1beta1.Queue{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "t.foobar.my-trigger.trigger-test-uid",
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind:       "Trigger",
						APIVersion: "eventing.knative.dev/v1",
						Name:       triggerName,
						UID:        triggerUID,
					},
				},
				Labels: map[string]string{
					"eventing.knative.dev/broker":  "testbroker",
					"eventing.knative.dev/trigger": "my-trigger",
				},
			},
			Spec: rabbitv1beta1.QueueSpec{
				Name:       "t.foobar.my-trigger.trigger-test-uid",
				Durable:    true,
				AutoDelete: false,
				RabbitmqClusterReference: rabbitv1beta1.RabbitmqClusterReference{
					Name: rabbitmqcluster,
				},
				Arguments: getTriggerQueueArguments(),
			},
		},
	}, {
		name:    "Trigger binding, filter and Delivery",
		broker:  createBroker(),
		trigger: createTriggerWithFilterAndDelivery(),
		want: &rabbitv1beta1.Queue{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      "t.foobar.my-trigger.trigger-test-uid",
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind:       "Trigger",
						APIVersion: "eventing.knative.dev/v1",
						Name:       triggerName,
						UID:        triggerUID,
					},
				},
				Labels: map[string]string{
					"eventing.knative.dev/broker":  "testbroker",
					"eventing.knative.dev/trigger": "my-trigger",
				},
			},
			Spec: rabbitv1beta1.QueueSpec{
				Name:       "t.foobar.my-trigger.trigger-test-uid",
				Durable:    true,
				AutoDelete: false,
				RabbitmqClusterReference: rabbitv1beta1.RabbitmqClusterReference{
					Name: rabbitmqcluster,
				},
				Arguments: getTriggerQueueArgumentsWithDeadLetterSink(),
			},
		},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			got := resources.NewQueue(context.TODO(), tt.broker, tt.trigger)
			if !equality.Semantic.DeepDerivative(tt.want, got) {
				t.Errorf("Unexpected Queue resource: want:\n%+v\ngot:\n%+v", tt.want, got)
			}
		})
	}
}

func TestNewTriggerDLQ(t *testing.T) {
	want := &rabbitv1beta1.Queue{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "t.foobar.my-trigger.dlq.trigger-test-uid",
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       "Trigger",
					APIVersion: "eventing.knative.dev/v1",
					Name:       triggerName,
					UID:        triggerUID,
				},
			},
			Labels: map[string]string{
				"eventing.knative.dev/broker":  "testbroker",
				"eventing.knative.dev/trigger": "my-trigger",
			},
		},
		Spec: rabbitv1beta1.QueueSpec{
			Name:       "t.foobar.my-trigger.dlq.trigger-test-uid",
			Durable:    true,
			AutoDelete: false,
			RabbitmqClusterReference: rabbitv1beta1.RabbitmqClusterReference{
				Name: rabbitmqcluster,
			},
		},
	}
	got := resources.NewTriggerDLQ(context.TODO(), createBroker(), createTriggerWithFilterAndDelivery())
	if !equality.Semantic.DeepDerivative(want, got) {
		t.Errorf("Unexpected Queue resource: want:\n%+v\ngot:\n%+v", want, got)
	}

}

func getTriggerQueueArguments() *runtime.RawExtension {
	arguments := map[string]string{
		"x-dead-letter-exchange": naming.BrokerExchangeName(createBroker(), true),
	}
	argumentsJson, err := json.Marshal(arguments)
	if err != nil {
		panic("Failed to marshal json for test, no go.")
	}
	return &runtime.RawExtension{
		Raw: argumentsJson,
	}
}

func getTriggerQueueArgumentsWithDeadLetterSink() *runtime.RawExtension {
	arguments := map[string]string{
		"x-dead-letter-exchange": naming.TriggerDLXExchangeName(createTriggerWithFilterAndDelivery()),
	}
	argumentsJson, err := json.Marshal(arguments)
	if err != nil {
		panic("Failed to marshal json for test, no go.")
	}
	return &runtime.RawExtension{
		Raw: argumentsJson,
	}
}
