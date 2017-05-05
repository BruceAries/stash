package controller

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	rapi "github.com/appscode/k8s-addons/api"
	tcs "github.com/appscode/k8s-addons/client/clientset"
	"github.com/appscode/log"
	"gopkg.in/robfig/cron.v2"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/client/cache"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/watch"
)

func NewCronController() (*cronController, error) {
	factory := cmdutil.NewFactory(nil)
	config, err := factory.ClientConfig()
	if err != nil {
		return nil, err
	}
	client, err := factory.ClientSet()
	if err != nil {
		return nil, err
	}
	return &cronController{
		extClient:     tcs.NewACExtensionsForConfigOrDie(config),
		kubeClient:    client,
		namespace:     os.Getenv(RestikNamespace),
		tprName:       os.Getenv(RestikResourceName),
		crons:         cron.New(),
		eventRecorder: NewEventRecorder(client, "Restik sidecar Watcher"),
	}, nil
}

func (cronWatcher *cronController) RunBackup() error {
	cronWatcher.crons.Start()
	lw := &cache.ListWatch{
		ListFunc: func(opts api.ListOptions) (runtime.Object, error) {
			return cronWatcher.extClient.Backups(cronWatcher.namespace).List(api.ListOptions{})
		},
		WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
			return cronWatcher.extClient.Backups(cronWatcher.namespace).Watch(api.ListOptions{})
		},
	}
	_, cronController := cache.NewInformer(lw,
		&rapi.Backup{},
		time.Minute*2,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if b, ok := obj.(*rapi.Backup); ok {
					if b.Name == cronWatcher.tprName {
						cronWatcher.backup = b
						err := cronWatcher.startCronBackupProcedure()
						if err != nil {
							log.Errorln(err)
						}
					}
				}
			},
			UpdateFunc: func(old, new interface{}) {
				oldObj, ok := old.(*rapi.Backup)
				if !ok {
					log.Errorln(errors.New("Error validating backup object"))
					return
				}
				newObj, ok := new.(*rapi.Backup)
				if !ok {
					log.Errorln(errors.New("Error validating backup object"))
					return
				}
				if !reflect.DeepEqual(oldObj.Spec, newObj.Spec) && newObj.Name == cronWatcher.tprName {
					cronWatcher.backup = newObj
					err := cronWatcher.startCronBackupProcedure()
					if err != nil {
						log.Errorln(err)
					}
				}
			},
		})
	cronController.Run(wait.NeverStop)
	return nil
}

func (cronWatcher *cronController) startCronBackupProcedure() error {
	backup := cronWatcher.backup
	password, err := getPasswordFromSecret(cronWatcher.kubeClient, backup.Spec.Destination.RepositorySecretName, backup.Namespace)
	if err != nil {
		return err
	}
	err = os.Setenv(RESTIC_PASSWORD, password)
	if err != nil {
		return err
	}
	repo := backup.Spec.Destination.Path
	_, err = os.Stat(filepath.Join(repo, "config"))
	if os.IsNotExist(err) {
		if _, err = execLocal(fmt.Sprintf("/restic init --repo %s", repo)); err != nil {
			return err
		}
	}
	// Remove previous jobs
	for _, v := range cronWatcher.crons.Entries() {
		cronWatcher.crons.Remove(v.ID)
	}
	interval := backup.Spec.Schedule
	if _, err = cron.Parse(interval); err != nil {
		log.Errorln(err)
		cronWatcher.eventRecorder.Event(backup, api.EventTypeWarning, EventReasonInvalidCronExpression, err.Error())
		//Reset Wrong Schedule
		backup.Spec.Schedule = ""
		_, err = cronWatcher.extClient.Backups(backup.Namespace).Update(backup)
		if err != nil {
			return err
		}
		cronWatcher.eventRecorder.Event(backup, api.EventTypeNormal, EventReasonSuccessfulCronExpressionReset, "Cron expression reset")
		return nil
	}
	_, err = cronWatcher.crons.AddFunc(interval, func() {
		if err := cronWatcher.runCronJob(); err != nil {
			log.Errorln(err)
			cronWatcher.eventRecorder.Event(backup, api.EventTypeWarning, EventReasonFailedCronJob, err.Error())
		}
	})
	if err != nil {
		return err
	}
	return nil
}

func (cronWatcher *cronController) runCronJob() error {
	backup := cronWatcher.backup
	password, err := getPasswordFromSecret(cronWatcher.kubeClient, cronWatcher.backup.Spec.Destination.RepositorySecretName, backup.Namespace)
	if err != nil {
		return err
	}
	err = os.Setenv(RESTIC_PASSWORD, password)
	if err != nil {
		return err
	}
	backupStartTime := unversioned.Now()
	cmd := fmt.Sprintf("/restic -r %s backup %s", backup.Spec.Destination.Path, backup.Spec.Source.Path)
	// add tags if any
	for _, t := range backup.Spec.Tags {
		cmd = cmd + " --tag " + t
	}
	// Force flag
	cmd = cmd + " --" + Force
	// Take Backup
	reason := ""
	errMessage := ""
	_, err = execLocal(cmd)
	if err != nil {
		log.Errorln("Restik backup failed cause ", err)
		errMessage = " ERROR: " + err.Error()
		reason = EventReasonFailedToBackup
	} else {
		backup.Status.LastSuccessfullBackupTime = &backupStartTime
		reason = EventReasonSuccessfulBackup
	}
	backup.Status.BackupCount++
	message := "Backup operation number = " + strconv.Itoa(int(backup.Status.BackupCount))
	cronWatcher.eventRecorder.Event(backup, api.EventTypeNormal, reason, message+errMessage)
	backupEndTime := unversioned.Now()
	_, err = snapshotRetention(backup)
	if err != nil {
		log.Errorln("Snapshot retention failed cause ", err)
		cronWatcher.eventRecorder.Event(backup, api.EventTypeNormal, EventReasonFailedToRetention, message+" ERROR: "+err.Error())
	}
	backup.Status.LastBackupTime = &backupStartTime
	if reflect.DeepEqual(backup.Status.FirstBackupTime, time.Time{}) {
		backup.Status.FirstBackupTime = &backupStartTime
	}
	backup.Status.LastBackupDuration = backupEndTime.Sub(backupStartTime.Time).String()
	backup, err = cronWatcher.extClient.Backups(backup.Namespace).Update(backup)
	if err != nil {
		log.Errorln(err)
		cronWatcher.eventRecorder.Event(backup, api.EventTypeNormal, EventReasonFailedToUpdate, err.Error())
	}
	return nil
}

func snapshotRetention(b *rapi.Backup) (string, error) {
	cmd := fmt.Sprintf("/restic -r %s forget", b.Spec.Destination.Path)
	if b.Spec.RetentionPolicy.KeepLastSnapshots > 0 {
		cmd = fmt.Sprintf("%s --%s %d", cmd, rapi.KeepLast, b.Spec.RetentionPolicy.KeepLastSnapshots)
	}
	if b.Spec.RetentionPolicy.KeepHourlySnapshots > 0 {
		cmd = fmt.Sprintf("%s --%s %d", cmd, rapi.KeepHourly, b.Spec.RetentionPolicy.KeepHourlySnapshots)
	}
	if b.Spec.RetentionPolicy.KeepDailySnapshots > 0 {
		cmd = fmt.Sprintf("%s --%s %d", cmd, rapi.KeepDaily, b.Spec.RetentionPolicy.KeepDailySnapshots)
	}
	if b.Spec.RetentionPolicy.KeepWeeklySnapshots > 0 {
		cmd = fmt.Sprintf("%s --%s %d", cmd, rapi.KeepWeekly, b.Spec.RetentionPolicy.KeepWeeklySnapshots)
	}
	if b.Spec.RetentionPolicy.KeepMonthlySnapshots > 0 {
		cmd = fmt.Sprintf("%s --%s %d", cmd, rapi.KeepMonthly, b.Spec.RetentionPolicy.KeepMonthlySnapshots)
	}
	if b.Spec.RetentionPolicy.KeepYearlySnapshots > 0 {
		cmd = fmt.Sprintf("%s --%s %d", cmd, rapi.KeepYearly, b.Spec.RetentionPolicy.KeepYearlySnapshots)
	}
	if len(b.Spec.RetentionPolicy.KeepTags) != 0 {
		for _, t := range b.Spec.RetentionPolicy.KeepTags {
			cmd = cmd + " --keep-tag " + t
		}
	}
	if len(b.Spec.RetentionPolicy.RetainHostname) != 0 {
		cmd = cmd + " --hostname " + b.Spec.RetentionPolicy.RetainHostname
	}
	if len(b.Spec.RetentionPolicy.RetainTags) != 0 {
		for _, t := range b.Spec.RetentionPolicy.RetainTags {
			cmd = cmd + " --tag " + t
		}
	}
	output, err := execLocal(cmd)
	return output, err
}

func execLocal(s string) (string, error) {
	parts := strings.Fields(s)
	head := parts[0]
	parts = parts[1:]
	cmdOut, err := exec.Command(head, parts...).Output()
	return strings.TrimSuffix(string(cmdOut), "\n"), err
}