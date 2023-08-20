package mainhandler

import (
	"context"
	"fmt"
	"os"
	"regexp"

	"github.com/kubescape/go-logger"
	"github.com/kubescape/go-logger/helpers"
	cs "github.com/kubescape/operator/continuousscanning"
	"github.com/kubescape/operator/utils"
	"github.com/kubescape/operator/watcher"
	"github.com/panjf2000/ants/v2"
	"go.opentelemetry.io/otel"

	"github.com/armosec/armoapi-go/identifiers"
	"github.com/armosec/utils-go/boolutils"
	"github.com/armosec/utils-go/httputils"

	"github.com/armosec/armoapi-go/apis"

	uuid "github.com/google/uuid"
	v1 "github.com/kubescape/opa-utils/httpserver/apis/v1"
	utilsmetav1 "github.com/kubescape/opa-utils/httpserver/meta/v1"

	reporterlib "github.com/armosec/logger-go/system-reports/datastructures"
	pkgwlid "github.com/armosec/utils-k8s-go/wlid"
	"github.com/kubescape/k8s-interface/k8sinterface"
	kssc "github.com/kubescape/storage/pkg/generated/clientset/versioned"
)

type MainHandler struct {
	eventWorkerPool        *ants.PoolWithFunc
	k8sAPI                 *k8sinterface.KubernetesApi
	commandResponseChannel *commandResponseChannelData
}

type ActionHandler struct {
	command                apis.Command
	reporter               reporterlib.IReporter
	k8sAPI                 *k8sinterface.KubernetesApi
	commandResponseChannel *commandResponseChannelData
	wlid                   string
}

type waitFunc func()

var k8sNamesRegex *regexp.Regexp
var actionNeedToBeWaitOnStartUp = map[apis.NotificationPolicyType]waitFunc{}
var KubescapeHttpClient httputils.IHttpClient
var VulnScanHttpClient httputils.IHttpClient

func init() {
	var err error
	k8sNamesRegex, err = regexp.Compile("[^A-Za-z0-9-]+")
	if err != nil {
		logger.L().Fatal(err.Error(), helpers.Error(err))
	}

	actionNeedToBeWaitOnStartUp[apis.TypeScanImages] = waitForVulnScanReady
	actionNeedToBeWaitOnStartUp[apis.TypeRunKubescape] = waitForKubescapeReady
}

// CreateWebSocketHandler Create ws-handler obj
func NewMainHandler(sessionObj *chan utils.SessionObj, k8sAPI *k8sinterface.KubernetesApi) *MainHandler {

	commandResponseChannel := make(chan *CommandResponseData, 100)
	limitedGoRoutinesCommandResponseChannel := make(chan *timerData, 10)
	mainHandler := &MainHandler{
		k8sAPI:                 k8sAPI,
		commandResponseChannel: &commandResponseChannelData{commandResponseChannel: &commandResponseChannel, limitedGoRoutinesCommandResponseChannel: &limitedGoRoutinesCommandResponseChannel},
	}
	pool, _ := ants.NewPoolWithFunc(utils.ConcurrencyWorkers, func(i interface{}) {
		j := i.(utils.Job)

		mainHandler.HandleRequest(j)
	})
	mainHandler.eventWorkerPool = pool
	return mainHandler
}

// CreateWebSocketHandler Create ws-handler obj
func NewActionHandler(k8sAPI *k8sinterface.KubernetesApi, sessionObj *utils.SessionObj, commandResponseChannel *commandResponseChannelData) *ActionHandler {
	return &ActionHandler{
		reporter:               sessionObj.Reporter,
		command:                sessionObj.Command,
		k8sAPI:                 k8sAPI,
		commandResponseChannel: commandResponseChannel,
	}
}

// SetupContinuousScanning sets up the continuous cluster scanning function
func (mainHandler *MainHandler) SetupContinuousScanning(ctx context.Context) error {
	clusterName := utils.ClusterConfig.ClusterName
	triggeringHandler := cs.NewTriggeringHandler(mainHandler.eventWorkerPool, clusterName)
	dynClient := mainHandler.k8sAPI.DynamicClient

	rulesFilename := utils.MatchingRulesFilename
	rulesReader, err := os.Open(rulesFilename)
	if err != nil {
		return err
	}

	fetcher := cs.NewFileFetcher(rulesReader)
	loader := cs.NewTargetLoader(fetcher)
	svc := cs.NewContinuousScanningService(dynClient, loader, triggeringHandler)
	svc.Launch(ctx)

	return nil
}

func (mainHandler *MainHandler) HandleWatchers(ctx context.Context) {
	defer func() {
		if err := recover(); err != nil {
			logger.L().Ctx(ctx).Fatal("recover in HandleWatchers", helpers.Interface("error", err))
		}
	}()

	ksStorageClient, err := kssc.NewForConfig(k8sinterface.GetK8sConfig())
	if err != nil {
		logger.L().Ctx(ctx).Fatal(fmt.Sprintf("Unable to initialize the storage client: %v", err))
	}
	watchHandler, err := watcher.NewWatchHandler(ctx, mainHandler.k8sAPI, ksStorageClient, nil, nil)

	if err != nil {
		logger.L().Ctx(ctx).Error(err.Error(), helpers.Error(err))
		return
	}

	// wait for vuln scan to be ready
	logger.L().Ctx(ctx).Info("Waiting for vuln scan to be ready")
	waitFunc := isActionNeedToWait(apis.Command{CommandName: apis.TypeScanImages})
	waitFunc()

	// generate list of commands to scan all workloads
	wlids := watchHandler.GetWlidsToContainerToImageIDMap()
	commandsList := []*apis.Command{}
	for wlid := range wlids {
		cmd := buildScanCommandForWorkload(ctx, wlid, watchHandler.GetContainerToImageIDForWlid(wlid), apis.TypeScanImages)
		commandsList = append(commandsList, cmd)
	}

	// insert commands to channel
	mainHandler.insertCommandsToChannel(ctx, commandsList)

	// start watching
	go watchHandler.PodWatch(ctx, mainHandler.eventWorkerPool)
	go watchHandler.SBOMWatch(ctx, mainHandler.eventWorkerPool)
	go watchHandler.SBOMFilteredWatch(ctx, mainHandler.eventWorkerPool)
	go watchHandler.VulnerabilityManifestWatch(ctx, mainHandler.eventWorkerPool)
}

func (h *MainHandler) StartContinuousScanning(ctx context.Context) error {
	return nil
}

func (mainHandler *MainHandler) insertCommandsToChannel(ctx context.Context, commandsList []*apis.Command) {
	for _, cmd := range commandsList {
		utils.AddCommandToChannel(ctx, cmd, mainHandler.eventWorkerPool)
	}
}

func buildScanCommandForWorkload(ctx context.Context, wlid string, mapContainerToImageID map[string]string, command apis.NotificationPolicyType) *apis.Command {
	return &apis.Command{
		Wlid:        wlid,
		CommandName: command,
		Args:        map[string]interface{}{utils.ContainerToImageIdsArg: mapContainerToImageID},
	}
}

// HandlePostmanRequest Parse received commands and run the command
func (mainHandler *MainHandler) HandleRequest(j utils.Job) {

	ctx := j.Context()
	sessionObj := j.Obj()

	// recover
	defer func() {
		if err := recover(); err != nil {
			logger.L().Ctx(ctx).Fatal("recover in HandleRequest", helpers.Interface("error", err))
		}
	}()

	ctx, span := otel.Tracer("").Start(ctx, string(sessionObj.Command.CommandName))

	// the all user experience depends on this line(the user/backend must get the action name in order to understand the job report)
	sessionObj.Reporter.SetActionName(string(sessionObj.Command.CommandName))

	isToItemizeScopeCommand := sessionObj.Command.WildWlid != "" || sessionObj.Command.WildSid != "" || len(sessionObj.Command.Designators) > 0
	switch sessionObj.Command.CommandName {
	case apis.TypeRunKubescape, apis.TypeRunKubescapeJob, apis.TypeSetKubescapeCronJob, apis.TypeDeleteKubescapeCronJob, apis.TypeUpdateKubescapeCronJob:
		isToItemizeScopeCommand = false

	case apis.TypeSetVulnScanCronJob, apis.TypeDeleteVulnScanCronJob, apis.TypeUpdateVulnScanCronJob:
		isToItemizeScopeCommand = false
	}

	if isToItemizeScopeCommand {
		mainHandler.HandleScopedRequest(ctx, &sessionObj) // this might be a heavy action, do not send to a goroutine
	} else {
		// handle requests
		mainHandler.HandleSingleRequest(ctx, &sessionObj)
	}
	span.End()
	close(sessionObj.ErrChan)
}

func (mainHandler *MainHandler) HandleSingleRequest(ctx context.Context, sessionObj *utils.SessionObj) {
	ctx, span := otel.Tracer("").Start(ctx, "mainHandler.HandleSingleRequest")
	defer span.End()

	actionHandler := NewActionHandler(mainHandler.k8sAPI, sessionObj, mainHandler.commandResponseChannel)
	actionHandler.reporter.SetActionName(string(sessionObj.Command.CommandName))
	actionHandler.reporter.SendDetails("Handling single request", true, sessionObj.ErrChan)
	err := actionHandler.runCommand(ctx, sessionObj)
	if err != nil {
		logger.L().Ctx(ctx).Error("failed to complete action", helpers.String("command", string(sessionObj.Command.CommandName)), helpers.String("wlid", sessionObj.Command.GetID()), helpers.Error(err))
		actionHandler.reporter.SendError(err, true, true, sessionObj.ErrChan)
		return
	}

	actionHandler.reporter.SendStatus(reporterlib.JobDone, true, sessionObj.ErrChan)
	logger.L().Ctx(ctx).Info("action completed successfully", helpers.String("command", string(sessionObj.Command.CommandName)), helpers.String("wlid", sessionObj.Command.GetID()))

}

func (actionHandler *ActionHandler) runCommand(ctx context.Context, sessionObj *utils.SessionObj) error {
	c := sessionObj.Command
	if pkgwlid.IsWlid(c.GetID()) {
		actionHandler.wlid = c.GetID()
	}

	switch c.CommandName {
	case apis.TypeScanImages:
		return actionHandler.scanWorkload(ctx, sessionObj)
	case apis.TypeRunKubescape, apis.TypeRunKubescapeJob:
		return actionHandler.kubescapeScan(ctx)
	case apis.TypeSetKubescapeCronJob:
		return actionHandler.setKubescapeCronJob(ctx)
	case apis.TypeUpdateKubescapeCronJob:
		return actionHandler.updateKubescapeCronJob(ctx)
	case apis.TypeDeleteKubescapeCronJob:
		return actionHandler.deleteKubescapeCronJob(ctx)
	case apis.TypeSetVulnScanCronJob:
		return actionHandler.setVulnScanCronJob(ctx)
	case apis.TypeUpdateVulnScanCronJob:
		return actionHandler.updateVulnScanCronJob(ctx)
	case apis.TypeDeleteVulnScanCronJob:
		return actionHandler.deleteVulnScanCronJob(ctx)
	case apis.TypeSetRegistryScanCronJob:
		return actionHandler.setRegistryScanCronJob(ctx, sessionObj)
	case apis.TypeScanRegistry:
		return actionHandler.scanRegistries(ctx, sessionObj)
	case apis.TypeTestRegistryConnectivity:
		return actionHandler.testRegistryConnectivity(ctx, sessionObj)
	case apis.TypeUpdateRegistryScanCronJob:
		return actionHandler.updateRegistryScanCronJob(ctx, sessionObj)
	case apis.TypeDeleteRegistryScanCronJob:
		return actionHandler.deleteRegistryScanCronJob(ctx)
	default:
		logger.L().Ctx(ctx).Error(fmt.Sprintf("Command %s not found", c.CommandName))
	}
	return nil
}

// HandleScopedRequest handle a request of a scope e.g. all workloads in a namespace
func (mainHandler *MainHandler) HandleScopedRequest(ctx context.Context, sessionObj *utils.SessionObj) {
	ctx, span := otel.Tracer("").Start(ctx, "mainHandler.HandleScopedRequest")
	defer span.End()

	if sessionObj.Command.GetID() == "" {
		logger.L().Ctx(ctx).Error("Received empty id")
		return
	}

	namespaces := make([]string, 0, 1)
	namespaces = append(namespaces, pkgwlid.GetNamespaceFromWlid(sessionObj.Command.GetID()))
	labels := sessionObj.Command.GetLabels()
	fields := sessionObj.Command.GetFieldSelector()
	if len(sessionObj.Command.Designators) > 0 {
		namespaces = make([]string, 0, 3)
		for desiIdx := range sessionObj.Command.Designators {
			if ns, ok := sessionObj.Command.Designators[desiIdx].Attributes[identifiers.AttributeNamespace]; ok {
				namespaces = append(namespaces, ns)
			}
		}
	}
	if len(namespaces) == 0 {
		namespaces = append(namespaces, "")
	}
	info := fmt.Sprintf("%s: id: '%s', namespaces: '%v', labels: '%v', fieldSelector: '%v'", sessionObj.Command.CommandName, sessionObj.Command.GetID(), namespaces, labels, fields)
	logger.L().Info(info)
	sessionObj.Reporter.SendDetails(info, true, sessionObj.ErrChan)
	ids, errs := mainHandler.getIDs(namespaces, labels, fields, []string{"pods"})
	for i := range errs {
		logger.L().Ctx(ctx).Warning(errs[i].Error())
		sessionObj.Reporter.SendError(errs[i], true, true, sessionObj.ErrChan)
	}

	sessionObj.Reporter.SendStatus(reporterlib.JobSuccess, true, sessionObj.ErrChan)

	logger.L().Info(fmt.Sprintf("ids found: '%v'", ids))

	for i := range ids {
		cmd := sessionObj.Command.DeepCopy()

		var err error
		if pkgwlid.IsWlid(ids[i]) {
			cmd.Wlid = ids[i]
			err = pkgwlid.IsWlidValid(cmd.Wlid)
		} else {
			err = fmt.Errorf("unknown id")
		}

		// clean all scope request parameters
		cmd.WildWlid = ""
		cmd.Designators = make([]identifiers.PortalDesignator, 0)

		// send specific command to the channel
		newSessionObj := utils.NewSessionObj(ctx, cmd, "Websocket", sessionObj.Reporter.GetJobID(), "", 1)

		if err != nil {
			err := fmt.Errorf("invalid: %s, id: '%s'", err.Error(), newSessionObj.Command.GetID())
			logger.L().Ctx(ctx).Error(err.Error())
			sessionObj.Reporter.SendError(err, true, true, sessionObj.ErrChan)
			continue
		}

		logger.L().Info("triggering", helpers.String("id", newSessionObj.Command.GetID()))
		mainHandler.HandleSingleRequest(ctx, newSessionObj)

		close(newSessionObj.ErrChan)

	}
}

func (mainHandler *MainHandler) getIDs(namespaces []string, labels, fields map[string]string, resources []string) ([]string, []error) {
	ids := []string{}
	errs := []error{}
	for _, resource := range resources {
		workloads, err := mainHandler.listWorkloads(namespaces, resource, labels, fields)
		if err != nil {
			errs = append(errs, err)
		}
		if len(workloads) == 0 {
			continue
		}
		w, e := mainHandler.getResourcesIDs(workloads)
		if len(e) != 0 {
			errs = append(errs, e...)
		}
		if len(w) == 0 {
			err := fmt.Errorf("resource: '%s', failed to calculate workloadIDs. namespaces: '%v', labels: '%v'", resource, namespaces, labels)
			errs = append(errs, err)
		}
		ids = append(ids, w...)
	}

	return ids, errs
}

// HandlePostmanRequest Parse received commands and run the command
func (mainHandler *MainHandler) StartupTriggerActions(ctx context.Context, actions []apis.Command) {

	for i := range actions {
		go func(index int) {
			waitFunc := isActionNeedToWait(actions[index])
			waitFunc()
			sessionObj := utils.NewSessionObj(ctx, &actions[index], "Websocket", "", uuid.NewString(), 1)
			l := utils.Job{}
			l.SetContext(ctx)
			l.SetObj(*sessionObj)
			if err := mainHandler.eventWorkerPool.Invoke(l); err != nil {
				logger.L().Ctx(ctx).Error("Failed to invoke job", helpers.String("wlid", actions[index].GetID()), helpers.String("command", fmt.Sprintf("%v", actions[index].CommandName)), helpers.String("args", fmt.Sprintf("%v", actions[index].Args)), helpers.Error(err))
			}
		}(i)
	}
}

func GetStartupActions() []apis.Command {
	return []apis.Command{
		{
			CommandName: apis.TypeRunKubescape,
			WildWlid:    pkgwlid.GetK8sWLID(utils.ClusterConfig.ClusterName, "", "", ""),
			Args: map[string]interface{}{
				utils.KubescapeScanV1: utilsmetav1.PostScanRequest{
					HostScanner: boolutils.BoolPointer(false),
					TargetType:  v1.KindFramework,
					TargetNames: []string{"allcontrols", "nsa", "mitre"},
				},
			},
		},
	}
}
