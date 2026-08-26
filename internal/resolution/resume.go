package resolution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/griffinbird/agentic-order-service/internal/investigation"
	"github.com/griffinbird/agentic-order-service/internal/orderdomain"
	"github.com/microsoft/agent-framework-go/workflow"
	"github.com/microsoft/agent-framework-go/workflow/checkpoint"
	"github.com/microsoft/agent-framework-go/workflow/inproc"
)

const manifestFileName = "run.json"

type EventFunc func(workflow.Event)

type PausedRun struct {
	SessionID  string                          `json:"sessionId"`
	Checkpoint workflow.CheckpointInfo         `json:"checkpoint"`
	RequestID  string                          `json:"requestId"`
	Approval   investigation.ApprovalRequest   `json:"approval"`
	Completed  bool                            `json:"completed"`
	Result     *investigation.ResolutionResult `json:"result,omitempty"`
}

func Start(
	ctx context.Context,
	wf *workflow.Workflow,
	stateDir string,
	orderID string,
	onEvent EventFunc,
) (PausedRun, error) {
	if wf == nil {
		return PausedRun{}, fmt.Errorf("workflow is required")
	}
	if err := orderdomain.ValidateID("state directory", stateDir); err != nil {
		return PausedRun{}, err
	}
	if err := orderdomain.ValidateID("order ID", orderID); err != nil {
		return PausedRun{}, err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return PausedRun{}, fmt.Errorf("create resolution state directory: %w", err)
	}
	store, err := checkpoint.NewFileSystemJSONStore(checkpointDir(stateDir))
	if err != nil {
		return PausedRun{}, fmt.Errorf("open checkpoint store: %w", err)
	}
	manager := checkpoint.NewJSONManager(store)
	run, err := inproc.Default.WithCheckpointing(manager).RunStreaming(ctx, wf, orderID)
	if err != nil {
		_ = store.Close()
		return PausedRun{}, fmt.Errorf("start resolution workflow: %w", err)
	}

	var paused PausedRun
	for event, eventErr := range run.WatchUntilHalt(ctx) {
		if eventErr != nil {
			err = eventErr
			break
		}
		if onEvent != nil {
			onEvent(event)
		}
		switch value := event.(type) {
		case workflow.RequestInfoEvent:
			request, ok := workflow.PortableValueAs[investigation.ApprovalRequest](value.Request.Data)
			if !ok {
				err = fmt.Errorf("unexpected approval request type %v", value.Request.PortInfo.RequestType)
				break
			}
			paused.RequestID = value.Request.RequestID
			paused.Approval = request
		case workflow.SuperStepCompletedEvent:
			if value.CompletionInfo != nil &&
				value.CompletionInfo.HasPendingRequests &&
				value.CompletionInfo.CheckpointInfo != nil {
				paused.Checkpoint = *value.CompletionInfo.CheckpointInfo
				paused.SessionID = value.CompletionInfo.CheckpointInfo.SessionID
			}
		case workflow.ErrorEvent:
			err = value.Error
		case workflow.ExecutorFailedEvent:
			err = value.Error
		}
		if err != nil {
			break
		}
	}
	err = errors.Join(err, run.Close(ctx), store.Close())
	if err != nil {
		return PausedRun{}, fmt.Errorf("pause resolution workflow: %w", err)
	}
	if paused.RequestID == "" || paused.SessionID == "" || paused.Checkpoint.CheckpointID == "" {
		return PausedRun{}, fmt.Errorf("workflow halted without a durable approval checkpoint")
	}
	if err := writeManifest(stateDir, paused); err != nil {
		return PausedRun{}, err
	}
	return paused, nil
}

func Resume(
	ctx context.Context,
	wf *workflow.Workflow,
	stateDir string,
	approved bool,
	actor string,
	permissions []string,
	onEvent EventFunc,
) (investigation.ResolutionResult, error) {
	if wf == nil {
		return investigation.ResolutionResult{}, fmt.Errorf("workflow is required")
	}
	paused, err := readManifest(stateDir)
	if err != nil {
		return investigation.ResolutionResult{}, err
	}
	if paused.Completed && paused.Result != nil {
		result := *paused.Result
		if result.Transfer != nil {
			transfer := *result.Transfer
			transfer.AlreadyApplied = true
			result.Transfer = &transfer
		}
		return result, nil
	}
	if approved {
		if err := orderdomain.ValidateID("approver", actor); err != nil {
			return investigation.ResolutionResult{}, err
		}
	}

	store, err := checkpoint.NewFileSystemJSONStore(checkpointDir(stateDir))
	if err != nil {
		return investigation.ResolutionResult{}, fmt.Errorf("open checkpoint store: %w", err)
	}
	index, err := store.RetrieveIndex(ctx, paused.SessionID, nil)
	if err != nil {
		_ = store.Close()
		return investigation.ResolutionResult{}, fmt.Errorf("read checkpoint index: %w", err)
	}
	if !containsCheckpoint(index, paused.Checkpoint) {
		_ = store.Close()
		return investigation.ResolutionResult{}, fmt.Errorf("saved approval checkpoint is missing")
	}
	manager := checkpoint.NewJSONManager(store)
	run, err := inproc.Default.WithCheckpointing(manager).ResumeStreaming(ctx, wf, paused.Checkpoint)
	if err != nil {
		_ = store.Close()
		return investigation.ResolutionResult{}, fmt.Errorf("resume resolution workflow: %w", err)
	}

	var result investigation.ResolutionResult
	var responded bool
	for event, eventErr := range run.WatchStream(ctx) {
		if eventErr != nil {
			err = eventErr
			break
		}
		if onEvent != nil {
			onEvent(event)
		}
		switch value := event.(type) {
		case workflow.RequestInfoEvent:
			if responded {
				err = fmt.Errorf("approval request %q was replayed more than once", value.Request.RequestID)
				break
			}
			if value.Request.RequestID != paused.RequestID {
				err = fmt.Errorf("approval request ID changed: got %q, want %q", value.Request.RequestID, paused.RequestID)
				break
			}
			request, ok := workflow.PortableValueAs[investigation.ApprovalRequest](value.Request.Data)
			if !ok {
				err = fmt.Errorf("unexpected replayed request type %v", value.Request.PortInfo.RequestType)
				break
			}
			digest, digestErr := request.Proposal.Digest()
			if digestErr != nil {
				err = digestErr
				break
			}
			response, responseErr := value.Request.CreateResponse(investigation.ApprovalResponse{
				Decision: orderdomain.ApprovalDecision{
					Approved:       approved,
					Actor:          actor,
					Permissions:    append([]string(nil), permissions...),
					ProposalDigest: digest,
					RequestID:      value.Request.RequestID,
				},
			})
			if responseErr != nil {
				err = responseErr
				break
			}
			if responseErr = run.SendResponse(ctx, response); responseErr != nil {
				err = responseErr
				break
			}
			responded = true
		case workflow.OutputEvent:
			typed, ok := value.Output.(investigation.ResolutionResult)
			if !ok {
				err = fmt.Errorf("unexpected resolution output type %T", value.Output)
				break
			}
			result = typed
		case workflow.ErrorEvent:
			err = value.Error
		case workflow.ExecutorFailedEvent:
			err = value.Error
		}
		if err != nil {
			break
		}
	}
	err = errors.Join(err, run.Close(ctx), store.Close())
	if err != nil {
		return investigation.ResolutionResult{}, fmt.Errorf("resume resolution workflow: %w", err)
	}
	if !responded || result.Status == "" {
		return investigation.ResolutionResult{}, fmt.Errorf("resumed workflow completed without a resolution")
	}
	paused.Completed = true
	paused.Result = &result
	if err := writeManifest(stateDir, paused); err != nil {
		return investigation.ResolutionResult{}, err
	}
	return result, nil
}

func checkpointDir(stateDir string) string {
	return filepath.Join(stateDir, "checkpoints")
}

func manifestPath(stateDir string) string {
	return filepath.Join(stateDir, manifestFileName)
}

func containsCheckpoint(index []workflow.CheckpointInfo, target workflow.CheckpointInfo) bool {
	for _, candidate := range index {
		if candidate == target {
			return true
		}
	}
	return false
}

func readManifest(stateDir string) (PausedRun, error) {
	data, err := os.ReadFile(manifestPath(stateDir))
	if err != nil {
		return PausedRun{}, fmt.Errorf("read resolution manifest: %w", err)
	}
	var paused PausedRun
	if err := json.Unmarshal(data, &paused); err != nil {
		return PausedRun{}, fmt.Errorf("decode resolution manifest: %w", err)
	}
	return paused, nil
}

func writeManifest(stateDir string, paused PausedRun) error {
	data, err := json.MarshalIndent(paused, "", "  ")
	if err != nil {
		return fmt.Errorf("encode resolution manifest: %w", err)
	}
	temp := manifestPath(stateDir) + ".tmp"
	if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write resolution manifest: %w", err)
	}
	if err := os.Rename(temp, manifestPath(stateDir)); err != nil {
		if removeErr := os.Remove(manifestPath(stateDir)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("remove previous resolution manifest: %w", removeErr)
		}
		if retryErr := os.Rename(temp, manifestPath(stateDir)); retryErr != nil {
			return fmt.Errorf("replace resolution manifest after removing previous file: %w", retryErr)
		}
	}
	return nil
}
