package psql_physical

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/docker/go-units"
	"github.com/hashicorp/go-multierror"

	"github.com/nixys/nxs-backup/ds/psql_connect"
	"github.com/nixys/nxs-backup/interfaces"
	"github.com/nixys/nxs-backup/misc"
	"github.com/nixys/nxs-backup/modules/backend/exec_cmd"
	"github.com/nixys/nxs-backup/modules/backend/targz"
	"github.com/nixys/nxs-backup/modules/logger"
	"github.com/nixys/nxs-backup/modules/metrics"
)

type job struct {
	name             string
	tmpDir           string
	needToMakeBackup bool
	safetyBackup     bool
	deferredCopying  bool
	diskRateLimit    int64
	storages         interfaces.Storages
	targets          map[string]target
	dumpedObjects    map[string]interfaces.DumpObject
	appMetrics       *metrics.Data
}

type target struct {
	connUrl   *url.URL
	extraKeys []string
	gzip      bool
}

type JobParams struct {
	Name             string
	TmpDir           string
	NeedToMakeBackup bool
	SafetyBackup     bool
	DeferredCopying  bool
	DiskRateLimit    int64
	Storages         interfaces.Storages
	Sources          []SourceParams
	Metrics          *metrics.Data
}

type SourceParams struct {
	Name          string
	ConnectParams psql_connect.Params
	ExtraKeys     []string
	Gzip          bool
	IsSlave       bool
}

func Init(jp JobParams) (interfaces.Job, error) {

	// check if mysqldump available
	if _, err := exec_cmd.Exec("pg_basebackup", "--version"); err != nil {
		return nil, fmt.Errorf("Job `%s` init failed. Can't check `pg_basebackup` version. Please install `pg_basebackup`. Error: %s ", jp.Name, err)
	}
	// check if tar and gzip available
	if _, err := exec_cmd.Exec("tar", "--version"); err != nil {
		return nil, fmt.Errorf("Job `%s` init failed. Can't check `tar` version. Please install `tar`. Error: %s ", jp.Name, err)
	}

	j := job{
		name:             jp.Name,
		tmpDir:           jp.TmpDir,
		needToMakeBackup: jp.NeedToMakeBackup,
		safetyBackup:     jp.SafetyBackup,
		deferredCopying:  jp.DeferredCopying,
		diskRateLimit:    jp.DiskRateLimit,
		storages:         jp.Storages,
		targets:          make(map[string]target),
		dumpedObjects:    make(map[string]interfaces.DumpObject),
		appMetrics: jp.Metrics.RegisterJob(
			metrics.JobData{
				JobName:       jp.Name,
				JobType:       misc.PostgresqlBasebackup,
				TargetMetrics: make(map[string]metrics.TargetData),
			},
		),
	}

	for _, src := range jp.Sources {

		for _, key := range src.ExtraKeys {
			if matched, _ := regexp.MatchString(`(-D|--pgdata=)`, key); matched {
				return nil, fmt.Errorf("Job `%s` init failed. Forbidden usage \"--pgdata|-D\" parameter as extra_keys for `postgresql_basebackup` jobs type ", jp.Name)
			}
		}

		cp := src.ConnectParams
		udb := strings.Split(src.ConnectParams.User, "@")
		if len(udb) > 1 {
			cp.Database = udb[1]
			cp.User = udb[0]
		}

		connUrl := psql_connect.GetConnUrl(cp)
		conn, err := psql_connect.GetConnect(connUrl)
		if err != nil {
			return nil, fmt.Errorf("Job `%s` init failed. PSQL connect error: %s ", jp.Name, err)
		}
		if err = conn.Ping(); err != nil {
			return nil, fmt.Errorf("Job `%s` init failed. PSQL ping check error: %s ", jp.Name, err)
		}
		_ = conn.Close()

		j.targets[src.Name] = target{
			extraKeys: src.ExtraKeys,
			gzip:      src.Gzip,
			connUrl:   connUrl,
		}
		j.appMetrics.Job[j.name].TargetMetrics[src.Name] = metrics.TargetData{
			Source: src.Name,
			Target: "",
			Values: make(map[string]float64),
		}
	}

	return &j, nil
}

func (j *job) SetOfsMetrics(ofs string, metricsMap map[string]float64) {
	for m, v := range metricsMap {
		j.appMetrics.Job[j.name].TargetMetrics[ofs].Values[m] = v
	}
}

func (j *job) GetName() string {
	return j.name
}

func (j *job) GetTempDir() string {
	return j.tmpDir
}

func (j *job) GetType() misc.BackupType {
	return misc.PostgresqlBasebackup
}

func (j *job) GetTargetOfsList() (ofsList []string) {
	for ofs := range j.targets {
		ofsList = append(ofsList, ofs)
	}
	return
}

func (j *job) GetStoragesCount() int {
	return len(j.storages)
}

func (j *job) GetDumpObjects() map[string]interfaces.DumpObject {
	return j.dumpedObjects
}

func (j *job) ListBackups() interfaces.JobTargets {
	jt := make(interfaces.JobTargets)

	for tn := range j.targets {
		jt[tn] = make(interfaces.TargetsOnStorages)
		jt[tn] = j.storages.ListBackups(tn)
	}

	return jt
}

func (j *job) SetDumpObjectDelivered(ofs string) {
	dumpObj := j.dumpedObjects[ofs]
	dumpObj.Delivered = true
	j.dumpedObjects[ofs] = dumpObj
}

func (j *job) IsBackupSafety() bool {
	return j.safetyBackup
}

func (j *job) NeedToMakeBackup() bool {
	return j.needToMakeBackup
}

func (j *job) NeedToUpdateIncMeta() bool {
	return false
}

func (j *job) DeleteOldBackups(logCh chan logger.LogRecord, ofsPath string) error {
	logCh <- logger.Log(j.name, "").Debugf("Starting rotate outdated backups.")
	return j.storages.DeleteOldBackups(logCh, j, ofsPath)
}

func (j *job) CleanupTmpData() error {
	return j.storages.CleanupTmpData(j)
}

func (j *job) DoBackup(logCh chan logger.LogRecord, tmpDir string) error {
	var errs *multierror.Error

	for ofsPart, tgt := range j.targets {
		startTime := time.Now()

		j.SetOfsMetrics(ofsPart, map[string]float64{
			metrics.BackupOk:        float64(0),
			metrics.BackupTime:      float64(0),
			metrics.DeliveryOk:      float64(0),
			metrics.DeliveryTime:    float64(0),
			metrics.BackupSize:      float64(0),
			metrics.BackupTimestamp: float64(startTime.Unix()),
		})

		tmpBackupFile := misc.GetFileFullPath(tmpDir, ofsPart, "tar", "", tgt.gzip)
		err := os.MkdirAll(path.Dir(tmpBackupFile), os.ModePerm)
		if err != nil {
			logCh <- logger.Log(j.name, "").Errorf("Unable to create tmp dir with next error: %s", err)
			errs = multierror.Append(errs, err)
			continue
		}

		if err = j.createTmpBackup(logCh, tmpBackupFile, ofsPart, tgt); err != nil {
			j.SetOfsMetrics(ofsPart, map[string]float64{
				metrics.BackupTime: float64(time.Since(startTime).Nanoseconds() / 1e6),
			})
			logCh <- logger.Log(j.name, "").Errorf("Failed to create temp backups %s", tmpBackupFile)
			errs = multierror.Append(errs, err)
			continue
		}
		fileInfo, _ := os.Stat(tmpBackupFile)
		j.SetOfsMetrics(ofsPart, map[string]float64{
			metrics.BackupOk:   float64(1),
			metrics.BackupTime: float64(time.Since(startTime).Nanoseconds() / 1e6),
			metrics.BackupSize: float64(fileInfo.Size()),
		})

		logCh <- logger.Log(j.name, "").Debugf("Created temp backups %s", tmpBackupFile)

		j.dumpedObjects[ofsPart] = interfaces.DumpObject{TmpFile: tmpBackupFile}

		if !j.deferredCopying {
			if err = j.storages.Delivery(logCh, j); err != nil {
				logCh <- logger.Log(j.name, "").Errorf("Failed to delivery backup. Errors: %v", err)
				errs = multierror.Append(errs, err)
			}
		}
	}

	if err := j.storages.Delivery(logCh, j); err != nil {
		logCh <- logger.Log(j.name, "").Errorf("Failed to delivery backup. Errors: %v", err)
		errs = multierror.Append(errs, err)
	}

	return errs.ErrorOrNil()
}

func (j *job) createTmpBackup(logCh chan logger.LogRecord, tmpBackupFile, tgtName string, tgt target) error {

	var stderr, stdout bytes.Buffer

	tmpBasebackupPath := path.Join(path.Dir(tmpBackupFile), "pg_basebackup_"+tgtName+"_"+misc.GetDateTimeNow(""))

	var args []string
	// define command args
	// add extra dump cmd options
	if len(tgt.extraKeys) > 0 {
		args = append(args, tgt.extraKeys...)
	}
	// add db connect
	args = append(args, "--dbname="+tgt.connUrl.String())
	// add data catalog path
	args = append(args, "--pgdata="+tmpBasebackupPath)
	args = append(args, "--format=plain")
	if j.diskRateLimit > 0 {
		maxRate := j.diskRateLimit / units.KB
		if maxRate < 32 {
			maxRate = 32
		} else if maxRate > 1024*units.KB {
			maxRate = 1024 * units.KB
		}
		args = append(args, fmt.Sprintf("--max-rate=%d", maxRate))
	}

	cmd := exec.Command("pg_basebackup", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	logCh <- logger.Log(j.name, "").Debugf("Dump cmd: %s", cmd.String())

	if err := cmd.Start(); err != nil {
		logCh <- logger.Log(j.name, "").Errorf("Unable to start pg_basebackup. Error: %s", err)
		return err
	}
	logCh <- logger.Log(j.name, "").Infof("Starting to dump `%s` source", tgtName)

	if err := cmd.Wait(); err != nil {
		logCh <- logger.Log(j.name, "").Errorf("Unable to make dump `%s`. Error: %s", tgtName, stderr.String())
		return err
	}
	logCh <- logger.Log(j.name, "").Debug("Got psql data. Compressing...")

	if err := targz.Tar(targz.TarOpts{
		Src:         tmpBasebackupPath,
		Dst:         tmpBackupFile,
		Incremental: false,
		Gzip:        tgt.gzip,
		SaveAbsPath: false,
		RateLim:     j.diskRateLimit,
		Excludes:    nil,
	}); err != nil {
		logCh <- logger.Log(j.name, "").Errorf("Unable to make tar: %s", err)
		var serr targz.Error
		if errors.As(err, &serr) {
			logCh <- logger.Log(j.name, "").Debugf("STDERR: %s", serr.Stderr)
		}
		return err
	}
	_ = os.RemoveAll(tmpBasebackupPath)

	logCh <- logger.Log(j.name, "").Infof("Dumping of source `%s` completed", tgtName)

	return nil
}

func (j *job) Close() error {
	for _, st := range j.storages {
		_ = st.Close()
	}
	return nil
}
