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

package trigger

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	"knative.dev/eventing-rabbitmq/third_party/pkg/apis/rabbitmq.com/v1beta1"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/logging"

	naming "knative.dev/eventing-rabbitmq/pkg/rabbitmqnaming"
	"knative.dev/eventing-rabbitmq/pkg/reconciler/trigger/resources"
	rabbitclientset "knative.dev/eventing-rabbitmq/third_party/pkg/client/clientset/versioned"
	rabbitlisters "knative.dev/eventing-rabbitmq/third_party/pkg/client/listers/rabbitmq.com/v1beta1"
	eventingduckv1 "knative.dev/eventing/pkg/apis/duck/v1"
	"knative.dev/eventing/pkg/apis/eventing"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	clientset "knative.dev/eventing/pkg/client/clientset/versioned"
	triggerreconciler "knative.dev/eventing/pkg/client/injection/reconciler/eventing/v1/trigger"
	eventinglisters "knative.dev/eventing/pkg/client/listers/eventing/v1"

	"knative.dev/eventing/pkg/duck"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/resolver"

	brokerresources "knative.dev/eventing-rabbitmq/pkg/reconciler/broker/resources"
)

type Reconciler struct {
	eventingClientSet clientset.Interface
	dynamicClientSet  dynamic.Interface
	kubeClientSet     kubernetes.Interface
	rabbitClientSet   rabbitclientset.Interface

	// listers index properties about resources
	deploymentLister appsv1listers.DeploymentLister
	brokerLister     eventinglisters.BrokerLister
	triggerLister    eventinglisters.TriggerLister
	exchangeLister   rabbitlisters.ExchangeLister
	queueLister      rabbitlisters.QueueLister
	bindingLister    rabbitlisters.BindingLister

	dispatcherImage              string
	dispatcherServiceAccountName string

	brokerClass string

	// Dynamic tracker to track KResources. In particular, it tracks the dependency between Triggers and Sources.
	sourceTracker duck.ListableTracker

	// Dynamic tracker to track AddressableTypes. In particular, it tracks Trigger subscribers.
	addressableTracker duck.ListableTracker
	uriResolver        *resolver.URIResolver
}

// Check that our Reconciler implements Interface
var _ triggerreconciler.Interface = (*Reconciler)(nil)

// isUsingOperator checks the Spec for a Broker and determines if we should be using the
// messaging-topology-operator or the libraries.
func isUsingOperator(b *eventingv1.Broker) bool {
	if b != nil && b.Spec.Config != nil {
		return b.Spec.Config.Kind == "RabbitmqCluster"
	}
	return false
}

func (r *Reconciler) ReconcileKind(ctx context.Context, t *eventingv1.Trigger) pkgreconciler.Event {
	logging.FromContext(ctx).Debug("Reconciling", zap.Any("Trigger", t))

	t.Status.InitializeConditions()

	broker, err := r.brokerLister.Brokers(t.Namespace).Get(t.Spec.Broker)
	if err != nil {
		if apierrs.IsNotFound(err) {
			t.Status.MarkBrokerFailed("BrokerDoesNotExist", "Broker %q does not exist", t.Spec.Broker)
			// Ok to return nil here. Once the Broker comes available, or Trigger changes, we get requeued.
			return nil
		}
		t.Status.MarkBrokerFailed("FailedToGetBroker", "Failed to get broker %q : %s", t.Spec.Broker, err)
		return fmt.Errorf("retrieving broker: %v", err)
	}

	// If it's not my brokerclass, ignore
	if broker.Annotations[eventing.BrokerClassKey] != r.brokerClass {
		logging.FromContext(ctx).Infof("Ignoring trigger %s/%s", t.Namespace, t.Name)
		return nil
	}

	t.Status.ObservedGeneration = t.Generation

	t.Status.PropagateBrokerCondition(broker.Status.GetTopLevelCondition())
	// If Broker is not ready, we're done, but once it becomes ready, we'll get requeued.
	if !broker.IsReady() {
		logging.FromContext(ctx).Errorw("Broker is not ready", zap.Any("Broker", broker))
		return nil
	}

	if err = r.checkDependencyAnnotation(ctx, t); err != nil {
		return err
	}

	// Just because there's no error, the dependency might not be ready, so check it.
	if ts := t.Status.GetCondition(eventingv1.TriggerConditionDependency); ts.Status != corev1.ConditionTrue {
		logging.FromContext(ctx).Info("Dependency is not ready")
		return nil
	}

	if !isUsingOperator(broker) {
		t.Status.MarkDependencyFailed("ReconcileFailure", "using secret is not supported with this controller")
		return nil
	} else {
		// If there's DeadLetterSink, we need to create a DLX that's specific for this Trigger as well
		// as a Queue for it, and Dispatcher that pulls from that queue.
		if t.Spec.Delivery != nil && t.Spec.Delivery.DeadLetterSink != nil {
			args := &brokerresources.ExchangeArgs{
				Broker:  broker,
				Trigger: t,
				DLX:     true,
			}
			dlx, err := r.reconcileExchange(ctx, args)
			if err != nil {
				t.Status.MarkDependencyFailed("ExchangeFailure", fmt.Sprintf("Failed to reconcile DLX exchange %q: %s", naming.TriggerDLXExchangeName(t), err))
				return err
			}
			if dlx != nil {
				if !isReady(dlx.Status.Conditions) {
					logging.FromContext(ctx).Warnf("DLX exchange %q is not ready", dlx.Name)
					t.Status.MarkDependencyFailed("ExchangeFailure", fmt.Sprintf("DLX exchange %q is not ready", dlx.Name))
					return nil
				}
			}

			dlq, err := r.reconcileDLQ(ctx, broker, t)
			if err != nil {
				logging.FromContext(ctx).Error("Problem reconciling Trigger Queue", zap.Error(err))
				t.Status.MarkDependencyFailed("QueueFailure", "%v", err)
				return err
			}
			if dlq != nil {
				if !isReady(dlq.Status.Conditions) {
					logging.FromContext(ctx).Warnf("DLQ %q is not ready", dlq.Name)
					t.Status.MarkDependencyFailed("QueueFailure", "DLQ %q is not ready", dlq.Name)
					return nil
				}
			}
			dlqBinding, err := r.reconcileDLQBinding(ctx, broker, t)
			if err != nil {
				logging.FromContext(ctx).Error("Problem reconciling Trigger DLQ Binding", zap.Error(err))
				t.Status.MarkDependencyFailed("BindingFailure", "%v", err)
				return err
			}
			if dlqBinding != nil {
				if !isReady(dlqBinding.Status.Conditions) {
					logging.FromContext(ctx).Warnf("DLQ Binding %q is not ready", dlqBinding.Name)
					t.Status.MarkDependencyFailed("BindingFailure", "DLQ Binding %q is not ready", dlqBinding.Name)
					return nil
				}
			}
			deadLetterSinkURI, err := r.uriResolver.URIFromDestinationV1(ctx, *t.Spec.Delivery.DeadLetterSink, t)
			if err != nil {
				logging.FromContext(ctx).Error("Unable to get the DeadLetterSink URI", zap.Error(err))
				t.Status.MarkDeadLetterSinkResolvedFailed("Unable to get the DeadLetterSink URI", "%v", err)
				t.Status.DeadLetterSinkURI = nil
				return err
			}
			t.Status.MarkDeadLetterSinkResolvedSucceeded()
			t.Status.DeadLetterSinkURI = deadLetterSinkURI
			_, err = r.reconcileDLXDispatcherDeployment(ctx, t, deadLetterSinkURI)
			if err != nil {
				logging.FromContext(ctx).Error("Problem reconciling DLX dispatcher Deployment", zap.Error(err))
				t.Status.MarkDependencyFailed("DeploymentFailure", "%v", err)
				return err
			}
		} else {
			// There's no Delivery spec, so just mark is as there's no DeadLetterSink Configured for it.
			t.Status.MarkDeadLetterSinkNotConfigured()
		}
		queue, err := r.reconcileQueue(ctx, broker, t)
		if err != nil {
			logging.FromContext(ctx).Error("Problem reconciling Trigger Queue", zap.Error(err))
			t.Status.MarkDependencyFailed("QueueFailure", "%v", err)
			return err
		}
		if queue != nil {
			if !isReady(queue.Status.Conditions) {
				logging.FromContext(ctx).Warnf("Queue %q is not ready", queue.Name)
				t.Status.MarkDependencyFailed("QueueFailure", "Queue %q is not ready", queue.Name)
				return nil
			}
		}

		logging.FromContext(ctx).Info("Reconciled rabbitmq queue", zap.Any("queue", queue))

		binding, err := r.reconcileBinding(ctx, broker, t)
		if err != nil {
			logging.FromContext(ctx).Error("Problem reconciling Trigger Queue Binding", zap.Error(err))
			t.Status.MarkDependencyFailed("BindingFailure", "%v", err)
			return err
		}
		if binding != nil {
			if !isReady(binding.Status.Conditions) {
				logging.FromContext(ctx).Warnf("Binding %q is not ready", binding.Name)
				t.Status.MarkDependencyFailed("BindingFailure", "Binding %q is not ready", binding.Name)
				return nil
			}
		}
		logging.FromContext(ctx).Info("Reconciled rabbitmq binding", zap.Any("binding", binding))
		t.Status.MarkDependencySucceeded()
	}
	if t.Spec.Subscriber.Ref != nil {
		// To call URIFromDestination(dest apisv1alpha1.Destination, parent interface{}), dest.Ref must have a Namespace
		// We will use the Namespace of Trigger as the Namespace of dest.Ref
		t.Spec.Subscriber.Ref.Namespace = t.GetNamespace()
	}

	subscriberURI, err := r.uriResolver.URIFromDestinationV1(ctx, t.Spec.Subscriber, t)
	if err != nil {
		logging.FromContext(ctx).Error("Unable to get the Subscriber's URI", zap.Error(err))
		t.Status.MarkSubscriberResolvedFailed("Unable to get the Subscriber's URI", "%v", err)
		t.Status.SubscriberURI = nil
		return err
	}
	t.Status.SubscriberURI = subscriberURI
	t.Status.MarkSubscriberResolvedSucceeded()

	// TODO no Subscription
	t.Status.PropagateSubscriptionCondition(
		&apis.Condition{
			Type:   "Ready",
			Status: "True",
		},
	)

	// If trigger specified delivery, use it.
	delivery := t.Spec.Delivery
	if delivery == nil {
		// If trigger didn't but Broker did, use it instead.
		delivery = broker.Spec.Delivery
	}

	_, err = r.reconcileDispatcherDeployment(ctx, t, subscriberURI, delivery)
	if err != nil {
		logging.FromContext(ctx).Error("Problem reconciling dispatcher Deployment", zap.Error(err))
		t.Status.MarkDependencyFailed("DeploymentFailure", "%v", err)
		return err
	}

	return nil
}

// reconcileDeployment reconciles the K8s Deployment 'd'.
func (r *Reconciler) reconcileDeployment(ctx context.Context, d *v1.Deployment) (*v1.Deployment, error) {
	current, err := r.deploymentLister.Deployments(d.Namespace).Get(d.Name)
	if apierrs.IsNotFound(err) {
		_, err = r.kubeClientSet.AppsV1().Deployments(d.Namespace).Create(ctx, d, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		return d, nil
	} else if err != nil {
		return nil, err
	} else if !equality.Semantic.DeepDerivative(d.Spec.Template, current.Spec.Template) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = d.Spec
		_, err = r.kubeClientSet.AppsV1().Deployments(desired.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
		if err != nil {
			return nil, err
		}
		return desired, nil
	}
	return current, nil
}

// reconcileDispatcherDeployment reconciles Trigger's dispatcher deployment.
func (r *Reconciler) reconcileDispatcherDeployment(ctx context.Context, t *eventingv1.Trigger, sub *apis.URL, delivery *eventingduckv1.DeliverySpec) (*v1.Deployment, error) {
	rabbitmqSecret, err := r.getRabbitmqSecret(ctx, t)
	if err != nil {
		return nil, err
	}
	b, err := r.brokerLister.Brokers(t.Namespace).Get(t.Spec.Broker)
	if err != nil {
		return nil, err
	}
	expected := resources.MakeDispatcherDeployment(&resources.DispatcherArgs{
		Trigger:            t,
		Image:              r.dispatcherImage,
		RabbitMQSecretName: rabbitmqSecret.Name,
		QueueName:          naming.CreateTriggerQueueName(t),
		BrokerUrlSecretKey: brokerresources.BrokerURLSecretKey,
		BrokerIngressURL:   b.Status.Address.URL,
		Subscriber:         sub,
		Delivery:           delivery,
	})
	return r.reconcileDeployment(ctx, expected)
}

// reconcileDispatcherDeployment reconciles Trigger's dispatcher deployment.
func (r *Reconciler) reconcileDLXDispatcherDeployment(ctx context.Context, t *eventingv1.Trigger, sub *apis.URL) (*v1.Deployment, error) {
	rabbitmqSecret, err := r.getRabbitmqSecret(ctx, t)
	if err != nil {
		return nil, err
	}
	b, err := r.brokerLister.Brokers(t.Namespace).Get(t.Spec.Broker)
	if err != nil {
		return nil, err
	}
	expected := resources.MakeDispatcherDeployment(&resources.DispatcherArgs{
		Trigger:            t,
		Image:              r.dispatcherImage,
		RabbitMQSecretName: rabbitmqSecret.Name,
		QueueName:          naming.CreateTriggerDeadLetterQueueName(t),
		BrokerUrlSecretKey: brokerresources.BrokerURLSecretKey,
		BrokerIngressURL:   b.Status.Address.URL,
		Subscriber:         sub,
		DLX:                true,
	})
	return r.reconcileDeployment(ctx, expected)
}

func (r *Reconciler) checkDependencyAnnotation(ctx context.Context, t *eventingv1.Trigger) error {
	if dependencyAnnotation, ok := t.GetAnnotations()[eventingv1.DependencyAnnotation]; ok {
		dependencyObjRef, err := eventingv1.GetObjRefFromDependencyAnnotation(dependencyAnnotation)
		if err != nil {
			t.Status.MarkDependencyFailed("ReferenceError", "Unable to unmarshal objectReference from dependency annotation of trigger: %v", err)
			return fmt.Errorf("getting object ref from dependency annotation %q: %v", dependencyAnnotation, err)
		}
		trackKResource := r.sourceTracker.TrackInNamespace(ctx, t)
		// Trigger and its dependent source are in the same namespace, we already did the validation in the webhook.
		if err := trackKResource(dependencyObjRef); err != nil {
			return fmt.Errorf("tracking dependency: %v", err)
		}
		if err := r.propagateDependencyReadiness(ctx, t, dependencyObjRef); err != nil {
			return fmt.Errorf("propagating dependency readiness: %v", err)
		}
	} else {
		t.Status.MarkDependencySucceeded()
	}
	return nil
}

func (r *Reconciler) propagateDependencyReadiness(ctx context.Context, t *eventingv1.Trigger, dependencyObjRef corev1.ObjectReference) error {
	lister, err := r.sourceTracker.ListerFor(dependencyObjRef)
	if err != nil {
		t.Status.MarkDependencyUnknown("ListerDoesNotExist", "Failed to retrieve lister: %v", err)
		return fmt.Errorf("retrieving lister: %v", err)
	}
	dependencyObj, err := lister.ByNamespace(t.GetNamespace()).Get(dependencyObjRef.Name)
	if err != nil {
		if apierrs.IsNotFound(err) {
			t.Status.MarkDependencyFailed("DependencyDoesNotExist", "Dependency does not exist: %v", err)
		} else {
			t.Status.MarkDependencyUnknown("DependencyGetFailed", "Failed to get dependency: %v", err)
		}
		return fmt.Errorf("getting the dependency: %v", err)
	}
	dependency := dependencyObj.(*duckv1.Source)

	// The dependency hasn't yet reconciled our latest changes to
	// its desired state, so its conditions are outdated.
	if dependency.GetGeneration() != dependency.Status.ObservedGeneration {
		logging.FromContext(ctx).Info("The ObjectMeta Generation of dependency is not equal to the observedGeneration of status",
			zap.Any("objectMetaGeneration", dependency.GetGeneration()),
			zap.Any("statusObservedGeneration", dependency.Status.ObservedGeneration))
		t.Status.MarkDependencyUnknown("GenerationNotEqual", "The dependency's metadata.generation, %q, is not equal to its status.observedGeneration, %q.", dependency.GetGeneration(), dependency.Status.ObservedGeneration)
		return nil
	}
	t.Status.PropagateDependencyStatus(dependency)
	return nil
}

func (r *Reconciler) getRabbitmqSecret(ctx context.Context, t *eventingv1.Trigger) (*corev1.Secret, error) {
	return r.kubeClientSet.CoreV1().Secrets(t.Namespace).Get(ctx, brokerresources.SecretName(t.Spec.Broker), metav1.GetOptions{})
}

func (r *Reconciler) reconcileExchange(ctx context.Context, args *brokerresources.ExchangeArgs) (*v1beta1.Exchange, error) {
	want := brokerresources.NewExchange(ctx, args)
	current, err := r.exchangeLister.Exchanges(args.Trigger.Namespace).Get(naming.TriggerDLXExchangeName(args.Trigger))
	if apierrs.IsNotFound(err) {
		logging.FromContext(ctx).Debugw("Creating rabbitmq exchange", zap.String("exchange name", want.Name))
		return r.rabbitClientSet.RabbitmqV1beta1().Exchanges(args.Trigger.Namespace).Create(ctx, want, metav1.CreateOptions{})
	} else if err != nil {
		return nil, err
	} else if !equality.Semantic.DeepDerivative(want.Spec, current.Spec) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = want.Spec
		return r.rabbitClientSet.RabbitmqV1beta1().Exchanges(args.Trigger.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
	}
	return current, nil
}

func (r *Reconciler) reconcileQueue(ctx context.Context, b *eventingv1.Broker, t *eventingv1.Trigger) (*v1beta1.Queue, error) {
	queueName := naming.CreateTriggerQueueName(t)
	want := resources.NewQueue(ctx, b, t)
	current, err := r.queueLister.Queues(b.Namespace).Get(queueName)
	if apierrs.IsNotFound(err) {
		logging.FromContext(ctx).Debugw("Creating rabbitmq queue", zap.String("queue name", want.Name))
		return r.rabbitClientSet.RabbitmqV1beta1().Queues(b.Namespace).Create(ctx, want, metav1.CreateOptions{})
	} else if err != nil {
		return nil, err
	} else if !equality.Semantic.DeepDerivative(want.Spec, current.Spec) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = want.Spec
		return r.rabbitClientSet.RabbitmqV1beta1().Queues(b.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
	}
	return current, nil
}

func (r *Reconciler) reconcileDLQ(ctx context.Context, b *eventingv1.Broker, t *eventingv1.Trigger) (*v1beta1.Queue, error) {
	queueName := naming.CreateTriggerDeadLetterQueueName(t)
	want := resources.NewTriggerDLQ(ctx, b, t)
	current, err := r.queueLister.Queues(b.Namespace).Get(queueName)
	if apierrs.IsNotFound(err) {
		logging.FromContext(ctx).Debugw("Creating rabbitmq queue", zap.String("queue name", want.Name))
		return r.rabbitClientSet.RabbitmqV1beta1().Queues(b.Namespace).Create(ctx, want, metav1.CreateOptions{})
	} else if err != nil {
		return nil, err
	} else if !equality.Semantic.DeepDerivative(want.Spec, current.Spec) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = want.Spec
		return r.rabbitClientSet.RabbitmqV1beta1().Queues(b.Namespace).Update(ctx, desired, metav1.UpdateOptions{})
	}
	return current, nil
}

func (r *Reconciler) reconcileBinding(ctx context.Context, b *eventingv1.Broker, t *eventingv1.Trigger) (*v1beta1.Binding, error) {
	// We can use the same name for queue / binding to keep things simpler
	bindingName := naming.CreateTriggerQueueName(t)

	want, err := resources.NewBinding(ctx, b, t)
	if err != nil {
		return nil, fmt.Errorf("failed to create the binding spec: %w", err)
	}
	current, err := r.bindingLister.Bindings(b.Namespace).Get(bindingName)
	if apierrs.IsNotFound(err) {
		logging.FromContext(ctx).Infow("Creating rabbitmq binding", zap.String("binding name", want.Name))
		return r.rabbitClientSet.RabbitmqV1beta1().Bindings(b.Namespace).Create(ctx, want, metav1.CreateOptions{})
	} else if err != nil {
		return nil, err
	} else if !equality.Semantic.DeepDerivative(want.Spec, current.Spec) {
		return nil, fmt.Errorf("binding spec differs and it's immutable")
	}
	return current, nil
}

func (r *Reconciler) reconcileDLQBinding(ctx context.Context, b *eventingv1.Broker, t *eventingv1.Trigger) (*v1beta1.Binding, error) {
	// We can use the same name for queue / binding to keep things simpler
	bindingName := naming.CreateTriggerDeadLetterQueueName(t)

	want, err := resources.NewTriggerDLQBinding(ctx, b, t)
	if err != nil {
		return nil, fmt.Errorf("failed to create the DLQ binding spec: %w", err)
	}
	current, err := r.bindingLister.Bindings(b.Namespace).Get(bindingName)
	if apierrs.IsNotFound(err) {
		logging.FromContext(ctx).Infow("Creating rabbitmq DLQ binding", zap.String("binding name", want.Name))
		return r.rabbitClientSet.RabbitmqV1beta1().Bindings(b.Namespace).Create(ctx, want, metav1.CreateOptions{})
	} else if err != nil {
		return nil, err
	} else if !equality.Semantic.DeepDerivative(want.Spec, current.Spec) {
		return nil, fmt.Errorf("binding spec differs and it's immutable")
	}
	return current, nil
}

func isReady(conditions []v1beta1.Condition) bool {
	numConditions := len(conditions)
	// If there are no conditions at all, the resource probably hasn't been reconciled yet => not ready
	if numConditions == 0 {
		return false
	}
	for _, c := range conditions {
		if c.Status == corev1.ConditionTrue {
			numConditions--
		}
	}
	return numConditions == 0
}
