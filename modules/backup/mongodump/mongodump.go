package mongodump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"time"

	"github.com/hashicorp/go-multierror"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/nixys/nxs-backup/ds/mongo_connect"
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
	host        string
	connOpts    mongo_connect.Params
	dbName      string
	collections []string
	extraKeys   []string
	gzip        bool
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
	Name               string
	ConnectParams      mongo_connect.Params
	TargetDBs          []string
	TargetCollections  []string
	ExcludeDBs         []string
	ExcludeCollections []string
	ExtraKeys          []string
	Gzip               bool
}

func Init(jp JobParams) (interfaces.Job, error) {

	// check if mysqldump available
	if _, err := exec_cmd.Exec("mongodump", "--version"); err != nil {
		return nil, fmt.Errorf("Job `%s` init failed. Can't check `mongodump` version. Please install `mongodump`. Error: %s ", jp.Name, err)
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
				JobType:       misc.MongoDB,
				TargetMetrics: make(map[string]metrics.TargetData),
			},
		),
	}

	for _, src := range jp.Sources {

		conn, host, err := mongo_connect.GetConnectAndHost(src.ConnectParams)
		if err != nil {
			return nil, fmt.Errorf("Job `%s` init failed. MongoDB connect error: %s ", jp.Name, err)
		}
		defer func() { _ = conn.Disconnect(context.TODO()) }()

		// fetch databases list to make backup
		var databases []string
		if misc.Contains(src.TargetDBs, "all") {
			databases, err = conn.ListDatabaseNames(context.TODO(), bson.D{})
			if err != nil {
				return nil, fmt.Errorf("Job `%s` init failed. Unable to list databases. Error: %s ", jp.Name, err)
			}
		} else {
			databases = src.TargetDBs
		}

		isAllCollectionsFlag := false
		if misc.Contains(src.TargetCollections, "all") {
			isAllCollectionsFlag = true
		}

		for _, db := range databases {
			if misc.Contains(src.ExcludeDBs, db) {
				continue
			}

			var ignoreCollections []string
			compRegEx := regexp.MustCompile(`^(?P<db>` + db + `)\.(?P<collection>.*$)`)
			for _, excl := range src.ExcludeCollections {
				if match := compRegEx.FindStringSubmatch(excl); len(match) > 0 {
					ignoreCollections = append(ignoreCollections, match[2])
				}
			}

			var collections, tc []string
			if isAllCollectionsFlag {
				collections, err = conn.Database(db).ListCollectionNames(context.TODO(), bson.D{})
				if err != nil {
					return nil, fmt.Errorf("Job `%s` init failed. Unable to list collections of database `%s`. Error: %s ", jp.Name, db, err)
				}
			} else {
				collections = src.TargetCollections
			}

			for _, col := range collections {
				if !misc.Contains(ignoreCollections, col) {
					tc = append(tc, col)
				}
			}

			ofs := src.Name + "/" + db
			j.targets[ofs] = target{
				dbName:      db,
				collections: tc,
				host:        host,
				extraKeys:   src.ExtraKeys,
				gzip:        src.Gzip,
				connOpts:    src.ConnectParams,
			}
			j.appMetrics.Job[j.name].TargetMetrics[ofs] = metrics.TargetData{
				Source: src.Name,
				Target: db,
				Values: make(map[string]float64),
			}
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
	return misc.MongoDB
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

		if err := os.MkdirAll(path.Dir(tmpBackupFile), os.ModePerm); err != nil {
			logCh <- logger.Log(j.name, "").Errorf("Unable to create tmp dir with next error: %s", err)
			errs = multierror.Append(errs, err)
			continue
		}

		if err := j.createTmpBackup(logCh, tmpBackupFile, tgt); err != nil {
			j.SetOfsMetrics(ofsPart, map[string]float64{
				metrics.BackupTime: float64(time.Since(startTime).Nanoseconds() / 1e6),
			})
			logCh <- logger.Log(j.name, "").Errorf("Unable to create temp backups %s", tmpBackupFile)
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
			if err := j.storages.Delivery(logCh, j); err != nil {
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

func (j *job) createTmpBackup(logCh chan logger.LogRecord, tmpBackupFile string, target target) error {
	tmpMongodumpPath := path.Join(path.Dir(tmpBackupFile), "dump")
	defer func() { _ = os.RemoveAll(tmpMongodumpPath) }()

	var args []string
	// define command args
	// auth url
	args = append(args, "--host="+target.host)
	if target.connOpts.AuthDB != "" {
		args = append(args, "--authenticationDatabase="+target.connOpts.AuthDB)
	} else {
		args = append(args, "--authenticationDatabase=admin")
	}
	args = append(args, "--username="+target.connOpts.User)
	args = append(args, "--password="+target.connOpts.Passwd)
	// add db name
	args = append(args, "--db="+target.dbName)

	if target.connOpts.TLSCAFile != "" {
		args = append(args, "--ssl")
		args = append(args, "--sslCAFile="+target.connOpts.TLSCAFile)
	}
	// add extra dump cmd options
	if len(target.extraKeys) > 0 {
		args = append(args, target.extraKeys...)
	}
	// set output
	args = append(args, "--out="+tmpMongodumpPath)

	var stderr, stdout bytes.Buffer
	logCh <- logger.Log(j.name, "").Infof("Starting a `%s` dump", target.dbName)

	for _, col := range target.collections {
		argsCol := append(args, "--collection="+col)
		cmd := exec.Command("mongodump", argsCol...)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		logCh <- logger.Log(j.name, "").Debugf("Dump cmd: %s", cmd.String())
		if err := cmd.Run(); err != nil {
			logCh <- logger.Log(j.name, "").Errorf("Unable to dump `%s`. Error: %s", target.dbName, err)
			logCh <- logger.Log(j.name, "").Debugf("STDOUT: %s", stdout.String())
			logCh <- logger.Log(j.name, "").Debugf("STDERR: %s", stderr.String())
			return err
		}
		stdout.Reset()
		stderr.Reset()
	}

	if err := targz.Tar(targz.TarOpts{
		Src:         tmpMongodumpPath,
		Dst:         tmpBackupFile,
		Incremental: false,
		Gzip:        target.gzip,
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

	logCh <- logger.Log(j.name, "").Infof("Dump of `%s` completed", target.dbName)

	return nil
}

func (j *job) Close() error {
	for _, st := range j.storages {
		_ = st.Close()
	}
	return nil
}
