/*
Copyright 2023 Vyogo Technologies.

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

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	vyogotechv1alpha1 "github.com/vyogotech/frappe-operator/api/v1alpha1"
)

const siteBackupFinalizer = "vyogo.tech/finalizer"

// SiteBackupReconciler reconciles a SiteBackup object
type SiteBackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=vyogo.tech,resources=sitebackups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=vyogo.tech,resources=sitebackups/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=vyogo.tech,resources=sitebackups/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SiteBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	siteBackup := &vyogotechv1alpha1.SiteBackup{}
	if err := r.Get(ctx, req.NamespacedName, siteBackup); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle finalizer
	if siteBackup.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(siteBackup, siteBackupFinalizer) {
			controllerutil.AddFinalizer(siteBackup, siteBackupFinalizer)
			if err := r.Update(ctx, siteBackup); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		if controllerutil.ContainsFinalizer(siteBackup, siteBackupFinalizer) {
			if err := r.handleFinalizer(ctx, siteBackup); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(siteBackup, siteBackupFinalizer)
			if err := r.Update(ctx, siteBackup); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Find the associated FrappeSite
	siteList := &vyogotechv1alpha1.FrappeSiteList{}
	if err := r.List(ctx, siteList, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	var benchRef *vyogotechv1alpha1.NamespacedName
	for _, site := range siteList.Items {
		if site.Spec.SiteName == siteBackup.Spec.Site {
			benchRef = site.Spec.BenchRef
			break
		}
	}

	if benchRef == nil {
		err := fmt.Errorf("no FrappeSite found for site %s", siteBackup.Spec.Site)
		logger.Error(err, "cannot proceed with backup")
		return ctrl.Result{}, r.updateSiteBackupStatus(ctx, siteBackup, "Failed", err.Error(), "")
	}

	// Get the bench
	bench := &vyogotechv1alpha1.FrappeBench{}
	if err := r.Get(ctx, client.ObjectKey{Name: benchRef.Name, Namespace: benchRef.Namespace}, bench); err != nil {
		if errors.IsNotFound(err) {
			logger.Error(err, "referenced FrappeBench not found", "bench", benchRef.Name)
			return ctrl.Result{}, r.updateSiteBackupStatus(ctx, siteBackup, "Failed", fmt.Sprintf("FrappeBench %s not found", benchRef.Name), "")
		}
		return ctrl.Result{}, err
	}

	if siteBackup.Spec.Schedule == "" {
		return r.reconcileOneTimeBackup(ctx, siteBackup, bench)
	} else {
		return r.reconcileScheduledBackup(ctx, siteBackup, bench)
	}
}

func (r *SiteBackupReconciler) getS3CredentialSecret(ctx context.Context, siteBackup *vyogotechv1alpha1.SiteBackup) (*corev1.Secret, error) {
	if siteBackup.Spec.Storage == nil || siteBackup.Spec.Storage.S3 == nil {
		return nil, nil
	}
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{
		Name:      siteBackup.Spec.Storage.S3.CredentialSecretRef.Name,
		Namespace: siteBackup.Spec.Storage.S3.CredentialSecretRef.Namespace,
	}, secret)
	if err != nil {
		if siteBackup.Spec.Storage.S3.CredentialSecretRef.Namespace == "" {
			// Fallback to siteBackup namespace
			err = r.Get(ctx, client.ObjectKey{
				Name:      siteBackup.Spec.Storage.S3.CredentialSecretRef.Name,
				Namespace: siteBackup.Namespace,
			}, secret)
		}
	}
	return secret, err
}

func (r *SiteBackupReconciler) handleFinalizer(ctx context.Context, siteBackup *vyogotechv1alpha1.SiteBackup) error {
	logger := log.FromContext(ctx)
	jobName := siteBackup.Name + "-backup"

	if siteBackup.Spec.Schedule == "" {
		// One-time backup: delete Job
		job := &batchv1.Job{}
		err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: siteBackup.Namespace}, job)
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
		if err == nil {
			logger.Info("Deleting associated Job", "Job", job.Name)
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				return err
			}
		}
	} else {
		// Scheduled backup: delete CronJob
		cronJob := &batchv1.CronJob{}
		err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: siteBackup.Namespace}, cronJob)
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
		if err == nil {
			logger.Info("Deleting associated CronJob", "CronJob", cronJob.Name)
			if err := r.Delete(ctx, cronJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileOneTimeBackup handles one-time backup creation and status updates
func (r *SiteBackupReconciler) reconcileOneTimeBackup(ctx context.Context, siteBackup *vyogotechv1alpha1.SiteBackup, bench *vyogotechv1alpha1.FrappeBench) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	jobName := siteBackup.Name + "-backup"

	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: siteBackup.Namespace}, job)

	if errors.IsNotFound(err) {
		if siteBackup.Status.Phase == "Succeeded" || siteBackup.Status.Phase == "Failed" {
			// Job is finished, do not recreate
			return ctrl.Result{}, nil
		}
		job = r.buildBackupJob(siteBackup, bench)
		if err := r.Create(ctx, job); err != nil {
			logger.Error(err, "Failed to create backup job")
			return ctrl.Result{}, err
		}
		logger.Info("Created backup job", "job", job.Name)
		return ctrl.Result{}, r.updateSiteBackupStatus(ctx, siteBackup, "Running", "Backup job created", job.Name)
	}

	if err != nil {
		logger.Error(err, "Failed to get backup job")
		return ctrl.Result{}, err
	}

	if job.Status.Succeeded > 0 {
		if siteBackup.Status.Phase != "Succeeded" {
			return ctrl.Result{}, r.updateSiteBackupStatus(ctx, siteBackup, "Succeeded", "Backup completed successfully", job.Name)
		}
	} else if job.Status.Failed > 0 {
		if siteBackup.Status.Phase != "Failed" {
			return ctrl.Result{}, r.updateSiteBackupStatus(ctx, siteBackup, "Failed", "Backup job failed", job.Name)
		}
	} else {
		if siteBackup.Status.Phase != "Running" {
			return ctrl.Result{}, r.updateSiteBackupStatus(ctx, siteBackup, "Running", "Backup job running", job.Name)
		}
	}

	return ctrl.Result{}, nil
}

// reconcileScheduledBackup handles scheduled backup creation
func (r *SiteBackupReconciler) reconcileScheduledBackup(ctx context.Context, siteBackup *vyogotechv1alpha1.SiteBackup, bench *vyogotechv1alpha1.FrappeBench) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	cronJobName := siteBackup.Name + "-backup"

	desiredCronJob := r.buildBackupCronJob(siteBackup, bench)
	currentCronJob := &batchv1.CronJob{}
	err := r.Get(ctx, client.ObjectKey{Name: cronJobName, Namespace: siteBackup.Namespace}, currentCronJob)

	if errors.IsNotFound(err) {
		if err := r.Create(ctx, desiredCronJob); err != nil {
			logger.Error(err, "Failed to create backup cronjob")
			return ctrl.Result{}, err
		}
		logger.Info("Created backup cronjob", "cronjob", desiredCronJob.Name)
		return ctrl.Result{}, r.updateSiteBackupStatus(ctx, siteBackup, "Scheduled", "Scheduled backup created", desiredCronJob.Name)
	}

	if err != nil {
		logger.Error(err, "Failed to get backup cronjob")
		return ctrl.Result{}, err
	}

	// CronJob exists, check if it needs updating
	if !reflect.DeepEqual(desiredCronJob.Spec, currentCronJob.Spec) {
		currentCronJob.Spec = desiredCronJob.Spec
		if err := r.Update(ctx, currentCronJob); err != nil {
			logger.Error(err, "Failed to update backup cronjob")
			return ctrl.Result{}, err
		}
		logger.Info("Updated backup cronjob", "cronjob", currentCronJob.Name)
		return ctrl.Result{}, r.updateSiteBackupStatus(ctx, siteBackup, "Scheduled", "Scheduled backup updated", currentCronJob.Name)
	}

	if siteBackup.Status.Phase != "Scheduled" {
		return ctrl.Result{}, r.updateSiteBackupStatus(ctx, siteBackup, "Scheduled", "Scheduled backup active", currentCronJob.Name)
	}

	return ctrl.Result{}, nil
}

// buildBackupArgs creates the command arguments for the backup job
func (r *SiteBackupReconciler) buildBackupArgs(siteBackup *vyogotechv1alpha1.SiteBackup) []string {
	args := []string{"--site", siteBackup.Spec.Site, "backup"}
	if siteBackup.Spec.WithFiles {
		args = append(args, "--with-files")
	}
	if siteBackup.Spec.Compress {
		args = append(args, "--compress")
	}
	if siteBackup.Spec.BackupPath != "" {
		args = append(args, "--backup-path", siteBackup.Spec.BackupPath)
	}
	if siteBackup.Spec.BackupPathDB != "" {
		args = append(args, "--backup-path-db", siteBackup.Spec.BackupPathDB)
	}
	if siteBackup.Spec.BackupPathConf != "" {
		args = append(args, "--backup-path-conf", siteBackup.Spec.BackupPathConf)
	}
	if siteBackup.Spec.BackupPathFiles != "" {
		args = append(args, "--backup-path-files", siteBackup.Spec.BackupPathFiles)
	}
	if siteBackup.Spec.BackupPathPrivateFiles != "" {
		args = append(args, "--backup-path-private-files", siteBackup.Spec.BackupPathPrivateFiles)
	}
	if len(siteBackup.Spec.Exclude) > 0 {
		args = append(args, "--exclude", strings.Join(siteBackup.Spec.Exclude, ","))
	}
	if len(siteBackup.Spec.Include) > 0 {
		args = append(args, "--include", strings.Join(siteBackup.Spec.Include, ","))
	}
	if siteBackup.Spec.IgnoreBackupConf {
		args = append(args, "--ignore-backup-conf")
	}
	if siteBackup.Spec.Verbose {
		args = append(args, "--verbose")
	}
	return args
}

// buildBackupScript creates a shell script for the backup job, optionally including S3 upload
func (r *SiteBackupReconciler) buildBackupScript(siteBackup *vyogotechv1alpha1.SiteBackup) string {
	benchArgs := r.buildBackupArgs(siteBackup)
	benchCmd := strings.Join(benchArgs, " ")

	script := fmt.Sprintf(`#!/bin/bash
set -e

# Setup user for OpenShift compatibility (fixes getpwuid() error)
if ! whoami &>/dev/null; then
  export USER=frappe
  export LOGNAME=frappe
  # Try to add user to /etc/passwd if writable
  if [ -w /etc/passwd ]; then
    echo "frappe:x:$(id -u):0:frappe user:/home/frappe:/sbin/nologin" >> /etc/passwd
  fi
fi

cd /home/frappe/frappe-bench
echo "Starting Frappe site backup for: %s"
bench %s
`, siteBackup.Spec.Site, benchCmd)

	if siteBackup.Spec.Storage != nil && siteBackup.Spec.Storage.S3 != nil {
		script += fmt.Sprintf(`
echo "Uploading backup files to S3 bucket: %s"
python3 << 'PYTHON_SCRIPT'
import os, boto3, glob, sys

site_name = "%s"
bucket = os.getenv("S3_BUCKET")
region = os.getenv("S3_REGION")
endpoint = os.getenv("S3_ENDPOINT")
access_key = os.getenv("AWS_ACCESS_KEY_ID")
secret_key = os.getenv("AWS_SECRET_ACCESS_KEY")

s3 = boto3.client('s3', 
    region_name=region if region else None,
    endpoint_url=endpoint if endpoint else None,
    aws_access_key_id=access_key,
    aws_secret_access_key=secret_key)

# The default backup path is sites/{site}/private/backups/
backup_dir = os.path.join("sites", site_name, "private/backups")
if not os.path.exists(backup_dir):
    print(f"Error: Backup directory {backup_dir} not found")
    sys.exit(1)

# Find all files created in the last 10 minutes (or just the latest ones)
# bench backup creates several files if --with-files is used
import time
currentTime = time.time()
files_uploaded = 0

for filepath in glob.glob(os.path.join(backup_dir, "*")):
    if os.path.isfile(filepath) and (currentTime - os.path.getmtime(filepath)) < 600:
        filename = os.path.basename(filepath)
        s3_key = f"{site_name}/{filename}"
        print(f"Uploading {filename} to s3://{bucket}/{s3_key}...")
        s3.upload_file(filepath, bucket, s3_key)
        files_uploaded += 1

if files_uploaded == 0:
    print("Warning: No recent backup files found to upload")
else:
    print(f"Successfully uploaded {files_uploaded} files to S3")
PYTHON_SCRIPT
`, siteBackup.Spec.Storage.S3.Bucket, siteBackup.Spec.Site)
	}

	return script
}

// buildBackupJob creates a Job for one-time backup
func (r *SiteBackupReconciler) buildBackupJob(siteBackup *vyogotechv1alpha1.SiteBackup, bench *vyogotechv1alpha1.FrappeBench) *batchv1.Job {
	jobName := siteBackup.Name + "-backup"
	backupScript := r.buildBackupScript(siteBackup)

	env := []corev1.EnvVar{}
	if siteBackup.Spec.Storage != nil && siteBackup.Spec.Storage.S3 != nil {
		secretName := siteBackup.Spec.Storage.S3.CredentialSecretRef.Name
		env = append(env, corev1.EnvVar{
			Name:  "S3_BUCKET",
			Value: siteBackup.Spec.Storage.S3.Bucket,
		})
		if siteBackup.Spec.Storage.S3.Region != "" {
			env = append(env, corev1.EnvVar{
				Name:  "S3_REGION",
				Value: siteBackup.Spec.Storage.S3.Region,
			})
		}
		if siteBackup.Spec.Storage.S3.Endpoint != "" {
			env = append(env, corev1.EnvVar{
				Name:  "S3_ENDPOINT",
				Value: siteBackup.Spec.Storage.S3.Endpoint,
			})
		}
		env = append(env, corev1.EnvVar{
			Name: "AWS_ACCESS_KEY_ID",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "AWS_ACCESS_KEY_ID",
				},
			},
		})
		env = append(env, corev1.EnvVar{
			Name: "AWS_SECRET_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "AWS_SECRET_ACCESS_KEY",
				},
			},
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: siteBackup.Namespace,
			Labels: map[string]string{
				"app":        "frappe",
				"site":       siteBackup.Spec.Site,
				"backup":     "true",
				"backupType": "one-time",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: r.getPodSecurityContext(bench),
					Containers: []corev1.Container{
						{
							Name:    "backup",
							Image:   r.getBenchImage(bench),
							Command: []string{"bash", "-c"},
							Args:    []string{backupScript},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "sites",
									MountPath: "/home/frappe/frappe-bench/sites",
									SubPath:   "frappe-sites",
								},
							},
							Env:             env,
							SecurityContext: r.getContainerSecurityContext(bench),
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "sites",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: r.getSitesPVCName(bench),
								},
							},
						},
					},
				},
			},
		},
	}

	controllerutil.SetControllerReference(siteBackup, job, r.Scheme)
	return job
}

// buildBackupCronJob creates a CronJob for scheduled backup
func (r *SiteBackupReconciler) buildBackupCronJob(siteBackup *vyogotechv1alpha1.SiteBackup, bench *vyogotechv1alpha1.FrappeBench) *batchv1.CronJob {
	cronJobName := siteBackup.Name + "-backup"
	backupScript := r.buildBackupScript(siteBackup)

	env := []corev1.EnvVar{}
	if siteBackup.Spec.Storage != nil && siteBackup.Spec.Storage.S3 != nil {
		secretName := siteBackup.Spec.Storage.S3.CredentialSecretRef.Name
		env = append(env, corev1.EnvVar{
			Name:  "S3_BUCKET",
			Value: siteBackup.Spec.Storage.S3.Bucket,
		})
		if siteBackup.Spec.Storage.S3.Region != "" {
			env = append(env, corev1.EnvVar{
				Name:  "S3_REGION",
				Value: siteBackup.Spec.Storage.S3.Region,
			})
		}
		if siteBackup.Spec.Storage.S3.Endpoint != "" {
			env = append(env, corev1.EnvVar{
				Name:  "S3_ENDPOINT",
				Value: siteBackup.Spec.Storage.S3.Endpoint,
			})
		}
		env = append(env, corev1.EnvVar{
			Name: "AWS_ACCESS_KEY_ID",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "AWS_ACCESS_KEY_ID",
				},
			},
		})
		env = append(env, corev1.EnvVar{
			Name: "AWS_SECRET_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "AWS_SECRET_ACCESS_KEY",
				},
			},
		})
	}

	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronJobName,
			Namespace: siteBackup.Namespace,
			Labels: map[string]string{
				"app":        "frappe",
				"site":       siteBackup.Spec.Site,
				"backup":     "true",
				"backupType": "scheduled",
			},
		},
		Spec: batchv1.CronJobSpec{
			Schedule:          siteBackup.Spec.Schedule,
			ConcurrencyPolicy: batchv1.ForbidConcurrent,
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy:   corev1.RestartPolicyNever,
							SecurityContext: r.getPodSecurityContext(bench),
							Containers: []corev1.Container{
								{
									Name:    "backup",
									Image:   r.getBenchImage(bench),
									Command: []string{"bash", "-c"},
									Args:    []string{backupScript},
									VolumeMounts: []corev1.VolumeMount{
										{
											Name:      "sites",
											MountPath: "/home/frappe/frappe-bench/sites",
											SubPath:   "frappe-sites",
										},
									},
									Env:             env,
									SecurityContext: r.getContainerSecurityContext(bench),
								},
							},
							Volumes: []corev1.Volume{
								{
									Name: "sites",
									VolumeSource: corev1.VolumeSource{
										PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
											ClaimName: r.getSitesPVCName(bench),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	controllerutil.SetControllerReference(siteBackup, cronJob, r.Scheme)
	return cronJob
}

// Security context helpers replicated for now (to be centralized later)
func (r *SiteBackupReconciler) getPodSecurityContext(bench *vyogotechv1alpha1.FrappeBench) *corev1.PodSecurityContext {
	// Simple implementation for now, can be aligned with frappesite_controller later
	return &corev1.PodSecurityContext{
		RunAsNonRoot: boolPtr(true),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func (r *SiteBackupReconciler) getContainerSecurityContext(bench *vyogotechv1alpha1.FrappeBench) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		ReadOnlyRootFilesystem: boolPtr(false),
	}
}

// getBenchImage returns the image to use for the bench
func (r *SiteBackupReconciler) getBenchImage(bench *vyogotechv1alpha1.FrappeBench) string {
	if bench.Spec.ImageConfig != nil && bench.Spec.ImageConfig.Repository != "" {
		image := bench.Spec.ImageConfig.Repository
		if bench.Spec.ImageConfig.Tag != "" {
			image = fmt.Sprintf("%s:%s", image, bench.Spec.ImageConfig.Tag)
		} else if bench.Spec.FrappeVersion != "" {
			image = fmt.Sprintf("%s:%s", image, bench.Spec.FrappeVersion)
		}
		return image
	}
	// Default image
	return fmt.Sprintf("frappe/erpnext:%s", bench.Spec.FrappeVersion)
}

// getSitesPVCName returns the PVC name for sites volume
func (r *SiteBackupReconciler) getSitesPVCName(bench *vyogotechv1alpha1.FrappeBench) string {
	return fmt.Sprintf("%s-sites", bench.Name)
}

// updateSiteBackupStatus updates the status of a SiteBackup resource
func (r *SiteBackupReconciler) updateSiteBackupStatus(ctx context.Context, siteBackup *vyogotechv1alpha1.SiteBackup, phase, message, jobName string) error {
	// Re-get the latest version to avoid conflicts
	latest := &vyogotechv1alpha1.SiteBackup{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(siteBackup), latest); err != nil {
		return err
	}

	latest.Status.Phase = phase
	latest.Status.Message = message
	latest.Status.LastBackupJob = jobName

	if phase == "Succeeded" {
		latest.Status.LastBackup = metav1.Now()
	}

	return r.Status().Update(ctx, latest)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SiteBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vyogotechv1alpha1.SiteBackup{}).
		Owns(&batchv1.Job{}).
		Owns(&batchv1.CronJob{}).
		Complete(r)
}
