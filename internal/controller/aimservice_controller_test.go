// MIT License
//
// Copyright (c) 2025 Advanced Micro Devices, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package controller

import (
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func watchedExternalMetric(name, value string) autoscalingv2.MetricStatus {
	quantity := resource.MustParse(value)
	return autoscalingv2.MetricStatus{
		Type: autoscalingv2.ExternalMetricSourceType,
		External: &autoscalingv2.ExternalMetricStatus{
			Metric:  autoscalingv2.MetricIdentifier{Name: name},
			Current: autoscalingv2.MetricValueStatus{AverageValue: &quantity},
		},
	}
}

func watchedHPA() *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "keda-hpa-service-predictor",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 3,
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			ObservedGeneration: ptr.To(int64(2)),
			CurrentReplicas:    1,
			DesiredReplicas:    1,
			CurrentMetrics: []autoscalingv2.MetricStatus{
				watchedExternalMetric("s0-default-service", "1"),
				watchedExternalMetric("s1-default-service", "2"),
			},
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
				{
					Type:               autoscalingv2.ScalingActive,
					Status:             corev1.ConditionTrue,
					Reason:             "ValidMetricFound",
					Message:            "metric available",
					LastTransitionTime: metav1.NewTime(time.Unix(100, 0)),
				},
				{
					Type:   autoscalingv2.AbleToScale,
					Status: corev1.ConditionTrue,
					Reason: "SucceededRescale",
				},
			},
		},
	}
}

func TestHPARelevantChangePredicate_Update(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*autoscalingv2.HorizontalPodAutoscaler)
		want   bool
	}{
		{
			name: "current replicas",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.CurrentReplicas++
			},
			want: true,
		},
		{
			name: "desired replicas",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.DesiredReplicas++
			},
			want: true,
		},
		{
			name: "generation",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Generation++
			},
			want: true,
		},
		{
			name: "observed generation",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.ObservedGeneration = ptr.To(int64(3))
			},
			want: true,
		},
		{
			name: "minimum replicas",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Spec.MinReplicas = ptr.To(int32(2))
			},
			want: true,
		},
		{
			name: "maximum replicas",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Spec.MaxReplicas++
			},
			want: true,
		},
		{
			name: "s0 disappears",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.CurrentMetrics = hpa.Status.CurrentMetrics[1:]
			},
			want: true,
		},
		{
			name: "external metric appears",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.CurrentMetrics = append(
					hpa.Status.CurrentMetrics,
					watchedExternalMetric("s2-default-service", "3"),
				)
			},
			want: true,
		},
		{
			name: "s0 loses its value",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.CurrentMetrics[0].External.Current = autoscalingv2.MetricValueStatus{}
			},
			want: true,
		},
		{
			name: "current value representation changes",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				quantity := resource.MustParse("1")
				hpa.Status.CurrentMetrics[0].External.Current = autoscalingv2.MetricValueStatus{Value: &quantity}
			},
			want: false,
		},
		{
			name: "numeric values change",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.CurrentMetrics[0] = watchedExternalMetric("s0-default-service", "99")
			},
			want: false,
		},
		{
			name: "metric order changes",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.CurrentMetrics[0], hpa.Status.CurrentMetrics[1] = hpa.Status.CurrentMetrics[1], hpa.Status.CurrentMetrics[0]
			},
			want: false,
		},
		{
			name: "ScalingActive reason changes",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.Conditions[0].Reason = "FailedGetExternalMetric"
			},
			want: true,
		},
		{
			name: "AbleToScale status changes",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.Conditions[1].Status = corev1.ConditionFalse
			},
			want: true,
		},
		{
			name: "condition message and timestamp change",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.Conditions[0].Message = "new detail"
				hpa.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Unix(200, 0))
			},
			want: false,
		},
		{
			name: "condition order changes",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.Status.Conditions[0], hpa.Status.Conditions[1] = hpa.Status.Conditions[1], hpa.Status.Conditions[0]
			},
			want: false,
		},
		{
			name: "resource version only",
			mutate: func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
				hpa.ResourceVersion = "2"
			},
			want: false,
		},
	}

	pred := hpaRelevantChangePredicate()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldHPA := watchedHPA()
			newHPA := oldHPA.DeepCopy()
			tt.mutate(newHPA)

			got := pred.Update(event.UpdateEvent{ObjectOld: oldHPA, ObjectNew: newHPA})
			if got != tt.want {
				t.Errorf("Update()=%v, want %v", got, tt.want)
			}
		})
	}

	t.Run("s0 gains a value", func(t *testing.T) {
		oldHPA := watchedHPA()
		oldHPA.Status.CurrentMetrics[0].External.Current = autoscalingv2.MetricValueStatus{}
		newHPA := oldHPA.DeepCopy()
		newHPA.Status.CurrentMetrics[0] = watchedExternalMetric("s0-default-service", "1")

		if !pred.Update(event.UpdateEvent{ObjectOld: oldHPA, ObjectNew: newHPA}) {
			t.Error("Update()=false, want true")
		}
	})
}

func TestHPARelevantChangePredicate_EventTypes(t *testing.T) {
	pred := hpaRelevantChangePredicate()
	hpa := watchedHPA()

	if !pred.Create(event.CreateEvent{Object: hpa}) {
		t.Error("Create()=false, want true")
	}
	if !pred.Delete(event.DeleteEvent{Object: hpa}) {
		t.Error("Delete()=false, want true")
	}
	if pred.Generic(event.GenericEvent{Object: hpa}) {
		t.Error("Generic()=true, want false")
	}
	if pred.Update(event.UpdateEvent{
		ObjectOld: &corev1.ConfigMap{},
		ObjectNew: &corev1.ConfigMap{},
	}) {
		t.Error("Update(non-HPA)=true, want false")
	}
}
