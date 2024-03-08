/*
Copyright 2023 The KubeSphere Authors.

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

package task

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kkcorev1 "github.com/kubesphere/kubekey/v4/pkg/apis/core/v1"
	kubekeyv1 "github.com/kubesphere/kubekey/v4/pkg/apis/kubekey/v1"
	kubekeyv1alpha1 "github.com/kubesphere/kubekey/v4/pkg/apis/kubekey/v1alpha1"
	"github.com/kubesphere/kubekey/v4/pkg/converter"
	"github.com/kubesphere/kubekey/v4/pkg/modules"
	"github.com/kubesphere/kubekey/v4/pkg/project"
	"github.com/kubesphere/kubekey/v4/pkg/variable"
)

type taskController struct {
	schema         *runtime.Scheme
	client         ctrlclient.Client
	taskReconciler reconcile.Reconciler

	taskqueue     workqueue.RateLimitingInterface
	MaxConcurrent int
}

// AddTasks to taskqueue if Tasks is not completed
func (c *taskController) AddTasks(ctx context.Context, pipeline *kubekeyv1.Pipeline) error {
	var nsTasks = &kubekeyv1alpha1.TaskList{}

	if err := c.client.List(ctx, nsTasks, ctrlclient.InNamespace(pipeline.Namespace), ctrlclient.MatchingFields{
		kubekeyv1alpha1.TaskOwnerField: ctrlclient.ObjectKeyFromObject(pipeline).String(),
	}); err != nil {
		klog.V(4).ErrorS(err, "List tasks error", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline))
		return err
	}
	defer func() {
		for _, task := range nsTasks.Items {
			c.taskqueue.Add(ctrl.Request{NamespacedName: ctrlclient.ObjectKeyFromObject(&task)})
		}
		converter.CalculatePipelineStatus(nsTasks, pipeline)
	}()

	if len(nsTasks.Items) != 0 {
		// task has generated. add exist generated task to taskqueue.
		return nil
	}
	// generate tasks
	v, err := variable.GetVariable(variable.Options{
		Ctx:      ctx,
		Client:   c.client,
		Pipeline: *pipeline,
	})
	if err != nil {
		return err
	}

	klog.V(6).InfoS("deal project", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline))
	projectFs, err := project.New(project.Options{Pipeline: pipeline}).FS(ctx, true)
	if err != nil {
		klog.V(4).ErrorS(err, "Deal project error", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline))
		return err
	}

	// convert to transfer.Playbook struct
	pb, err := converter.MarshalPlaybook(projectFs, pipeline.Spec.Playbook)
	if err != nil {
		return err
	}

	// set pipeline location
	if err := v.Merge(variable.LocationMerge{
		UID:  string(pipeline.UID),
		Name: pipeline.Name,
	}); err != nil {
		klog.V(4).ErrorS(err, "set top location for pipeline", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline))
		return err
	}

	for _, play := range pb.Play {
		if !play.Taggable.IsEnabled(pipeline.Spec.Tags, pipeline.Spec.SkipTags) {
			// if not match the tags. skip
			continue
		}
		// hosts should contain all host's name. hosts should not be empty.
		var hosts []string
		if ahn, err := v.Get(variable.Hostnames{Name: play.PlayHost.Hosts}); err == nil {
			hosts = ahn.([]string)
		}
		if len(hosts) == 0 {
			klog.V(4).ErrorS(nil, "Hosts is empty", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline))
			return fmt.Errorf("hosts is empty")
		}

		// when gather_fact is set. get host's information from remote.
		if play.GatherFacts {
			for _, h := range hosts {
				gfv, err := getGatherFact(ctx, h, v)
				if err != nil {
					klog.V(4).ErrorS(err, "Get gather fact error", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline), "host", h)
					return err
				}
				// merge host information to runtime variable
				if err := v.Merge(variable.HostMerge{
					HostNames: []string{h},
					Data:      gfv,
				}); err != nil {
					klog.V(4).ErrorS(err, "Merge gather fact error", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline), "host", h)
					return err
				}
			}
		}

		// Batch execution, with each batch being a group of hosts run in serial.
		var batchHosts [][]string
		if play.RunOnce {
			// runOnce only run in first node
			batchHosts = [][]string{{hosts[0]}}
		} else {
			// group hosts by serial. run the playbook by serial
			batchHosts, err = converter.GroupHostBySerial(hosts, play.Serial.Data)
			if err != nil {
				klog.V(4).ErrorS(err, "Group host by serial error", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline))
				return err
			}
		}

		// generate task by each batch.
		for _, serials := range batchHosts {
			// each batch hosts should not be empty.
			if len(serials) == 0 {
				klog.V(4).ErrorS(nil, "Host is empty", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline))
				return fmt.Errorf("host is empty")
			}

			// generate playbook uid.
			uid := uuid.NewString()
			// set play location
			if err := v.Merge(variable.LocationMerge{
				ParentUID: string(pipeline.UID),
				UID:       uid,
				Name:      play.Name,
				Type:      variable.BlockLocation,
				Vars:      play.Vars,
			}); err != nil {
				klog.V(4).ErrorS(err, "set block location for play", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline), "play", play.Name)
				return err
			}
			// generate task from pre tasks
			preTasks, err := c.createTasks(ctx, createTasksOptions{
				variable:     v,
				pipeline:     pipeline,
				hosts:        serials,
				blocks:       play.PreTasks,
				uid:          uid,
				role:         "",
				when:         nil,
				locationType: variable.BlockLocation,
			})
			if err != nil {
				klog.V(4).ErrorS(err, "Get pre task from  play error", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline), "play", play.Name)
				return err
			}

			nsTasks.Items = append(nsTasks.Items, preTasks...)
			// generate task from role
			for _, role := range play.Roles {
				roleuid := uuid.NewString()
				if err := v.Merge(variable.LocationMerge{
					ParentUID: uid,
					UID:       roleuid,
					Name:      role.Role,
					Type:      variable.BlockLocation,
					Vars:      role.Vars,
				}); err != nil {
					klog.V(4).ErrorS(err, "set block location for role", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline), "play", play.Name, "role", role.Role)
					return err
				}
				roleTasks, err := c.createTasks(ctx, createTasksOptions{
					variable:     v,
					pipeline:     pipeline,
					hosts:        serials,
					blocks:       role.Block,
					uid:          roleuid,
					role:         role.Role,
					when:         role.When.Data,
					locationType: variable.BlockLocation,
				})
				if err != nil {
					klog.V(4).ErrorS(err, "Get role task from  play error", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline), "play", play.Name, "role", role.Role)
					return err
				}
				nsTasks.Items = append(nsTasks.Items, roleTasks...)
			}
			// generate task from tasks
			tasks, err := c.createTasks(ctx, createTasksOptions{
				variable:     v,
				pipeline:     pipeline,
				hosts:        serials,
				blocks:       play.Tasks,
				uid:          uid,
				role:         "",
				when:         nil,
				locationType: variable.BlockLocation,
			})
			if err != nil {
				klog.V(4).ErrorS(err, "Get task from  play error", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline), "play", play.Name)
				return err
			}
			nsTasks.Items = append(nsTasks.Items, tasks...)
			// generate task from post tasks
			postTasks, err := c.createTasks(ctx, createTasksOptions{
				variable:     v,
				pipeline:     pipeline,
				hosts:        serials,
				blocks:       play.Tasks,
				uid:          uid,
				role:         "",
				when:         nil,
				locationType: variable.BlockLocation,
			})
			if err != nil {
				klog.V(4).ErrorS(err, "Get post task from  play error", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline), "play", play.Name)
				return err
			}
			nsTasks.Items = append(nsTasks.Items, postTasks...)
		}
	}

	return nil
}

type createTasksOptions struct {
	// pipeline level config
	variable variable.Variable
	pipeline *kubekeyv1.Pipeline
	// playbook level config
	hosts []string // which hosts will run playbook
	// blocks level config
	blocks       []kkcorev1.Block
	uid          string                // the parent location uid for blocks
	role         string                // role name of blocks
	when         []string              // when condition for blocks
	locationType variable.LocationType // location type for blocks
}

// createTasks convert ansible block to task
func (k *taskController) createTasks(ctx context.Context, options createTasksOptions) ([]kubekeyv1alpha1.Task, error) {
	var tasks []kubekeyv1alpha1.Task
	for _, at := range options.blocks {
		if !at.Taggable.IsEnabled(options.pipeline.Spec.Tags, options.pipeline.Spec.SkipTags) {
			continue
		}

		switch {
		case len(at.Block) != 0:
			uid := uuid.NewString()
			// set block location
			if err := options.variable.Merge(variable.LocationMerge{
				UID:       uid,
				ParentUID: options.uid,
				Type:      options.locationType,
				Name:      at.Name,
				Vars:      at.Vars,
			}); err != nil {
				klog.V(4).ErrorS(err, "set block location for block", "pipeline", ctrlclient.ObjectKeyFromObject(options.pipeline), "block", at.Name)
				return nil, err
			}

			// add block
			blockTasks, err := k.createTasks(ctx, createTasksOptions{
				hosts:        options.hosts,
				role:         options.role,
				variable:     options.variable,
				pipeline:     options.pipeline,
				blocks:       at.Block,
				when:         append(options.when, at.When.Data...),
				uid:          uid,
				locationType: variable.BlockLocation,
			})
			if err != nil {
				klog.V(4).ErrorS(err, "Get block task from block error", "pipeline", ctrlclient.ObjectKeyFromObject(options.pipeline), "block", at.Name)
				return nil, err
			}
			tasks = append(tasks, blockTasks...)

			if len(at.Always) != 0 {
				alwaysTasks, err := k.createTasks(ctx, createTasksOptions{
					variable:     options.variable,
					pipeline:     options.pipeline,
					hosts:        options.hosts,
					blocks:       at.Always,
					uid:          uid,
					role:         options.role,
					when:         append(options.when, at.When.Data...),
					locationType: variable.AlwaysLocation,
				})
				if err != nil {
					klog.V(4).ErrorS(err, "Get always task from block error", "pipeline", ctrlclient.ObjectKeyFromObject(options.pipeline), "block", at.Name)
					return nil, err
				}
				tasks = append(tasks, alwaysTasks...)
			}
			if len(at.Rescue) != 0 {
				rescueTasks, err := k.createTasks(ctx, createTasksOptions{
					variable:     options.variable,
					pipeline:     options.pipeline,
					hosts:        options.hosts,
					blocks:       at.Rescue,
					uid:          uid,
					role:         options.role,
					when:         append(options.when, at.When.Data...),
					locationType: variable.RescueLocation,
				})
				if err != nil {
					klog.V(4).ErrorS(err, "Get rescue task from block error", "pipeline", ctrlclient.ObjectKeyFromObject(options.pipeline), "block", at.Name)
					return nil, err
				}
				tasks = append(tasks, rescueTasks...)
			}
		case at.IncludeTasks != "":
			// do nothing
			// set includeTask location
			if err := options.variable.Merge(variable.LocationMerge{
				UID:       uuid.NewString(),
				ParentUID: options.uid,
				Type:      options.locationType,
				Name:      at.IncludeTasks,
				Vars:      at.Vars,
			}); err != nil {
				klog.V(4).ErrorS(err, "set block location for includeTask", "pipeline", ctrlclient.ObjectKeyFromObject(options.pipeline), "block", at.Name)
				return nil, err
			}
		default:
			task := converter.MarshalBlock(ctx, options.role, options.hosts, append(options.when, at.When.Data...), at)
			// complete by pipeline
			task.GenerateName = options.pipeline.Name + "-"
			task.Namespace = options.pipeline.Namespace
			if err := controllerutil.SetControllerReference(options.pipeline, task, k.schema); err != nil {
				klog.V(4).ErrorS(err, "Set controller reference error", "pipeline", ctrlclient.ObjectKeyFromObject(options.pipeline), "block", at.Name)
				return nil, err
			}
			// complete module by unknown field
			for n, a := range at.UnknownFiled {
				data, err := json.Marshal(a)
				if err != nil {
					klog.V(4).ErrorS(err, "Marshal unknown field error", "pipeline", ctrlclient.ObjectKeyFromObject(options.pipeline), "block", at.Name, "field", n)
					return nil, err
				}
				if m := modules.FindModule(n); m != nil {
					task.Spec.Module.Name = n
					task.Spec.Module.Args = runtime.RawExtension{Raw: data}
					break
				}
			}
			if task.Spec.Module.Name == "" { // action is necessary for a task
				klog.V(4).ErrorS(nil, "No module/action detected in task", "pipeline", ctrlclient.ObjectKeyFromObject(options.pipeline), "block", at.Name)
				return nil, fmt.Errorf("no module/action detected in task: %s", task.Name)
			}
			// create task
			if err := k.client.Create(ctx, task); err != nil {
				klog.V(4).ErrorS(err, "Create task error", "pipeline", ctrlclient.ObjectKeyFromObject(options.pipeline), "block", at.Name)
				return nil, err
			}
			tasks = append(tasks, *task)

			// set task location
			if err := options.variable.Merge(variable.LocationMerge{
				UID:       string(task.UID),
				ParentUID: options.uid,
				Type:      options.locationType,
				Name:      at.Name,
				Vars:      at.Vars,
			}); err != nil {
				klog.V(4).ErrorS(err, "set block location for task", "pipeline", ctrlclient.ObjectKeyFromObject(options.pipeline), "block", at.Name)
				return nil, err
			}
		}
	}
	return tasks, nil
}

// Start task controller, deal task in work taskqueue
func (k *taskController) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		k.taskqueue.ShutDown()
	}()
	// deal work taskqueue
	wg := &sync.WaitGroup{}
	for i := 0; i < k.MaxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k.processNextWorkItem(ctx) {
			}
		}()
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (k *taskController) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := k.taskqueue.Get()
	if shutdown {
		return false
	}

	defer k.taskqueue.Done(obj)

	req, ok := obj.(ctrl.Request)
	if !ok {
		// As the item in the workqueue is actually invalid, we call
		// Forget here else we'd go into a loop of attempting to
		// process a work item that is invalid.
		k.taskqueue.Forget(obj)
		klog.V(4).ErrorS(nil, "Queue item was not a Request", "request", req)
		// Return true, don't take a break
		return true
	}

	result, err := k.taskReconciler.Reconcile(ctx, req)
	switch {
	case err != nil:
		k.taskqueue.AddRateLimited(req)
		klog.V(4).ErrorS(err, "Reconciler error", "request", req)
	case result.RequeueAfter > 0:
		// The result.RequeueAfter request will be lost, if it is returned
		// along with a non-nil error. But this is intended as
		// We need to drive to stable reconcile loops before queuing due
		// to result.RequestAfter
		k.taskqueue.Forget(obj)
		k.taskqueue.AddAfter(req, result.RequeueAfter)
	case result.Requeue:
		k.taskqueue.AddRateLimited(req)
	default:
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		k.taskqueue.Forget(obj)
	}
	return true
}

func (k *taskController) NeedLeaderElection() bool {
	return true
}
