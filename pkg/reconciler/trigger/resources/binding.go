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

package resources

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	naming "knative.dev/eventing-rabbitmq/pkg/rabbitmqnaming"
	rabbitv1beta1 "knative.dev/eventing-rabbitmq/third_party/pkg/apis/rabbitmq.com/v1beta1"
	"knative.dev/pkg/kmeta"

	"knative.dev/eventing/pkg/apis/eventing"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
)

const (
	DefaultManagementPort = 15672
	BindingKey            = "x-knative-trigger"
	DLQBindingKey         = "x-knative-dlq"
	TriggerDLQBindingKey  = "x-knative-trigger-dlq"
)

func NewBinding(ctx context.Context, broker *eventingv1.Broker, trigger *eventingv1.Trigger) (*rabbitv1beta1.Binding, error) {
	var or metav1.OwnerReference
	var bindingName string
	var sourceName string

	arguments := map[string]string{
		"x-match": "all",
	}
	// Depending on if we have a Broker & Trigger we need to rejigger some of the names and
	// configurations little differently, so do that up front before actually creating the resource
	// that we're returning.
	if trigger != nil {
		or = *kmeta.NewControllerRef(trigger)
		bindingName = naming.CreateTriggerQueueName(trigger)
		arguments[BindingKey] = trigger.Name
		sourceName = naming.BrokerExchangeName(broker, false)
		if trigger.Spec.Filter != nil && trigger.Spec.Filter.Attributes != nil {
			for key, val := range trigger.Spec.Filter.Attributes {
				arguments[key] = val
			}
		}
	} else {
		or = *kmeta.NewControllerRef(broker)
		bindingName = naming.CreateBrokerDeadLetterQueueName(broker)
		arguments[DLQBindingKey] = broker.Name
		sourceName = naming.BrokerExchangeName(broker, true)
	}

	argumentsJson, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("failed to encode binding arguments %+v : %s", argumentsJson, err)
	}

	binding := &rabbitv1beta1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       broker.Namespace,
			Name:            bindingName,
			OwnerReferences: []metav1.OwnerReference{or},
			Labels:          BindingLabels(broker, trigger),
		},
		Spec: rabbitv1beta1.BindingSpec{
			Vhost:           "/",
			Source:          sourceName,
			Destination:     bindingName,
			DestinationType: "queue",
			RoutingKey:      "",
			Arguments: &runtime.RawExtension{
				Raw: argumentsJson,
			},

			// TODO: We had before also internal / nowait set to false. Are these in Arguments,
			// or do they get sane defaults that we can just work with?
			// TODO: This one has to exist in the same namespace as this exchange.
			RabbitmqClusterReference: rabbitv1beta1.RabbitmqClusterReference{
				Name: broker.Spec.Config.Name,
			},
		},
	}
	return binding, nil
}

// NewTriggerDLQBinding creates a binding for a Trigger DLX.
func NewTriggerDLQBinding(ctx context.Context, broker *eventingv1.Broker, trigger *eventingv1.Trigger) (*rabbitv1beta1.Binding, error) {

	arguments := map[string]string{
		"x-match":            "all",
		TriggerDLQBindingKey: trigger.Name,
	}

	bindingName := naming.CreateTriggerDeadLetterQueueName(trigger)
	sourceName := naming.TriggerDLXExchangeName(trigger)
	argumentsJson, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("failed to encode DLQ binding arguments %+v : %s", argumentsJson, err)
	}

	binding := &rabbitv1beta1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       trigger.Namespace,
			Name:            bindingName,
			OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(trigger)},
			Labels:          BindingLabels(broker, trigger),
		},
		Spec: rabbitv1beta1.BindingSpec{
			Vhost:           "/",
			Source:          sourceName,
			Destination:     bindingName,
			DestinationType: "queue",
			RoutingKey:      "",
			Arguments: &runtime.RawExtension{
				Raw: argumentsJson,
			},

			// TODO: We had before also internal / nowait set to false. Are these in Arguments,
			// or do they get sane defaults that we can just work with?
			// TODO: This one has to exist in the same namespace as this exchange.
			RabbitmqClusterReference: rabbitv1beta1.RabbitmqClusterReference{
				Name: broker.Spec.Config.Name,
			},
		},
	}
	return binding, nil
}

// BindingLabels generates the labels present on the Queue linking the Broker / Trigger to the
// Binding.
func BindingLabels(b *eventingv1.Broker, t *eventingv1.Trigger) map[string]string {
	if t == nil {
		return map[string]string{
			eventing.BrokerLabelKey: b.Name,
		}
	} else {
		return map[string]string{
			eventing.BrokerLabelKey: b.Name,
			TriggerLabelKey:         t.Name,
		}
	}
}
