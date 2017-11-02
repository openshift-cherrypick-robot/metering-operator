package chargeback

import (
	"encoding/json"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	cbTypes "github.com/coreos-inc/kube-chargeback/pkg/apis/chargeback/v1alpha1"
	"github.com/coreos-inc/kube-chargeback/pkg/hive"
	"github.com/coreos-inc/kube-chargeback/pkg/presto"
)

var (
	defaultRunImmediately = true
	defaultGracePeriod    = metav1.Duration{Duration: time.Minute * 5}
)

func generateHiveColumns(report *cbTypes.Report, genQuery *cbTypes.ReportGenerationQuery) []hive.Column {
	columns := make([]hive.Column, 0)
	for _, c := range genQuery.Spec.Columns {
		columns = append(columns, hive.Column{Name: c.Name, Type: c.Type})
	}
	return columns
}

func (c *Chargeback) runReportWorker() {
	for c.processReport() {

	}
}

func (c *Chargeback) processReport() bool {
	key, quit := c.informers.reportQueue.Get()
	if quit {
		return false
	}
	defer c.informers.reportQueue.Done(key)

	err := c.syncReport(key.(string))
	c.handleErr(err, "report", key, c.informers.reportQueue)
	return true
}

func (c *Chargeback) syncReport(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.logger.WithError(err).Errorf("invalid resource key :%s", key)
		return nil
	}

	report, err := c.informers.reportLister.Reports(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.logger.Infof("Report %s does not exist anymore", key)
			return nil
		}
		return err
	}

	c.logger.Infof("syncing report %s", report.GetName())
	err = c.handleReport(report)
	if err != nil {
		c.logger.WithError(err).Errorf("error syncing report %s", report.GetName())
		return err
	}
	c.logger.Infof("successfully synced report %s", report.GetName())
	return nil
}

func (c *Chargeback) handleReport(report *cbTypes.Report) error {
	report = report.DeepCopy()

	logger := c.logger.WithFields(log.Fields{
		"name": report.Name,
	})
	switch report.Status.Phase {
	case cbTypes.ReportPhaseStarted:
		// If it's started, query the API to get the most up to date resource,
		// as it's possible it's finished, but we haven't gotten it yet.
		newReport, err := c.chargebackClient.ChargebackV1alpha1().Reports(report.Namespace).Get(report.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if report.UID != newReport.UID {
			logger.Warn("started report has different UUID in API than in cache, waiting for resync to process")
			return nil
		}

		c.informers.reportInformer.GetIndexer().Update(newReport)
		if err != nil {
			logger.WithError(err).Warnf("unable to update report cache with updated report")
			// if we cannot update it, don't re queue it
			return nil
		}

		// It's no longer started, requeue it
		if newReport.Status.Phase != cbTypes.ReportPhaseStarted {
			key, err := cache.MetaNamespaceKeyFunc(newReport)
			if err == nil {
				c.informers.reportQueue.AddRateLimited(key)
			}
			return nil
		}

		err = fmt.Errorf("unable to determine if report generation succeeded")
		c.setReportError(logger, report, err, "found already started report, report generation likely failed while processing")
		return nil
	case cbTypes.ReportPhaseFinished, cbTypes.ReportPhaseError:
		logger.Infof("ignoring report %s, status: %s", report.Name, report.Status.Phase)
		return nil
	default:

		logger.Infof("new report discovered")
	}

	// set the default grace period
	if report.Spec.GracePeriod == nil {
		report.Spec.GracePeriod = &defaultGracePeriod
	}

	// If we're waiting until the end and we're not past the end time + grace
	// period, ignore this report
	if !report.Spec.RunImmediately && report.Spec.ReportingEnd.Add(report.Spec.GracePeriod.Duration).After(time.Now()) {
		logger.Infof("report %s not past grace period yet, ignoring for now", report.Name)
		return nil
	}

	logger = logger.WithField("generationQuery", report.Spec.GenerationQueryName)
	genQuery, err := c.informers.reportGenerationQueryLister.ReportGenerationQueries(report.Namespace).Get(report.Spec.GenerationQueryName)
	if err != nil {
		logger.WithError(err).Errorf("failed to get report generation query")
		return err
	}

	logger = logger.WithFields(log.Fields{
		"reportStart": report.Spec.ReportingStart,
		"reportEnd":   report.Spec.ReportingEnd,
	})

	if valid, err := c.validateGenerationQuery(logger, genQuery); err != nil {
		c.setReportError(logger, report, err, "report is invalid")
		return nil
	} else if !valid {
		return nil
	}

	// update status
	report.Status.Phase = cbTypes.ReportPhaseStarted
	newReport, err := c.chargebackClient.ChargebackV1alpha1().Reports(report.Namespace).Update(report)
	if err != nil {
		logger.WithError(err).Errorf("failed to update report status to started for %q", report.Name)
		return err
	}
	report = newReport

	results, err := c.generateReport(logger, report, genQuery)
	if err != nil {
		// TODO(chance): return the error and handle retrying
		c.setReportError(logger, report, err, "report execution failed")
		return nil
	}
	if c.logReport {
		resultsJSON, err := json.MarshalIndent(results, "", " ")
		if err != nil {
			logger.WithError(err).Errorf("unable to marshal report into JSON")
			return nil
		}
		logger.Debugf("results: %s", string(resultsJSON))
	}

	// update status
	report.Status.Phase = cbTypes.ReportPhaseFinished
	_, err = c.chargebackClient.ChargebackV1alpha1().Reports(report.Namespace).Update(report)
	if err != nil {
		logger.WithError(err).Warnf("failed to update report status to finished for %q", report.Name)
	} else {
		logger.Infof("finished report %q", report.Name)
	}
	return nil
}

func (c *Chargeback) setReportError(logger *log.Entry, report *cbTypes.Report, err error, errMsg string) {
	logger.WithError(err).Errorf(errMsg)
	report.Status.Phase = cbTypes.ReportPhaseError
	report.Status.Output = err.Error()
	_, err = c.chargebackClient.ChargebackV1alpha1().Reports(report.Namespace).Update(report)
	if err != nil {
		logger.WithError(err).Errorf("unable to update report status to error")
	}
}

func (c *Chargeback) generateReport(logger *log.Entry, report *cbTypes.Report, genQuery *cbTypes.ReportGenerationQuery) ([]map[string]interface{}, error) {
	logger.Infof("generating usage report")
	query, err := renderReportGenerationQuery(report, genQuery)
	if err != nil {
		return nil, err
	}

	// Create a table to write to
	reportTable := reportTableName(report.Name)
	storage := report.Spec.Output
	switch {
	case storage == nil || storage.Local != nil:
		logger.Debugf("Creating table %s backed by local storage", reportTable)
		err = hive.CreateLocalReportTable(c.hiveQueryer, reportTable, generateHiveColumns(report, genQuery))
	case storage.S3 != nil:
		bucket, prefix := storage.S3.Bucket, storage.S3.Prefix
		logger.Debugf("Creating table %s pointing to s3 bucket %s at prefix %s", reportTable, bucket, prefix)
		err = hive.CreateReportTable(c.hiveQueryer, reportTable, bucket, prefix, generateHiveColumns(report, genQuery))
	default:
		return nil, fmt.Errorf("storage incorrectly configured on report %s", report.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("Couldn't create table for output report: %v", err)
	}

	logger.Debugf("deleting any preexisting rows in %s", reportTable)
	_, err = presto.ExecuteSelect(c.prestoConn, fmt.Sprintf("DELETE FROM %s", reportTable))
	if err != nil {
		return nil, fmt.Errorf("couldn't empty table %s of preexisting rows: %v", reportTable, err)
	}

	// Run the report
	logger.Debugf("running report generation query")
	err = presto.ExecuteInsertQuery(c.prestoConn, reportTable, query)
	if err != nil {
		logger.WithError(err).Errorf("creating usage report FAILED!")
		return nil, fmt.Errorf("Failed to execute %s usage report: %v", genQuery.Name, err)
	}

	getReportQuery := fmt.Sprintf("SELECT * FROM %s", reportTable)
	results, err := presto.ExecuteSelect(c.prestoConn, getReportQuery)
	if err != nil {
		logger.WithError(err).Errorf("getting usage report FAILED!")
		return nil, fmt.Errorf("Failed to get usage report results: %v", err)
	}
	return results, nil
}