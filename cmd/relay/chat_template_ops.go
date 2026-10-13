package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/project"
)

const (
	auditCredentialChatTemplate = "chat_template"
	maxChatTemplateNameRunes    = 60
	maxChatTemplateModelBytes   = 200
)

// ChatTemplateOps is the one core behind the chat-template add door. It adds a
// template that carries a name and a model and nothing else: a start can name
// it, so the add is gated like an agent entry.
type ChatTemplateOps struct {
	Store config.SettingsStore
	Queue *config.CommandQueue
	// Gate is the presence check Add demands. A nil Gate refuses.
	Gate     *presence.Gate
	Issuance IssuanceAuditor
}

type ChatTemplateInput struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

type ChatTemplateView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

type ChatTemplateAddResult struct {
	ProjectID string           `json:"projectId"`
	Template  ChatTemplateView `json:"template"`
}

// newChatTemplateOps shares ProjectOps's queue, gate and auditor. This is
// deliberate: project writes then run in one queue lane.
func newChatTemplateOps(p *ProjectOps) *ChatTemplateOps {
	return &ChatTemplateOps{Store: p.Store, Queue: p.Queue, Gate: p.Gate, Issuance: p.Issuance}
}

func validateChatTemplateInput(in ChatTemplateInput) *AgentError {
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > maxChatTemplateNameRunes ||
		strings.TrimSpace(in.Name) == "" || strings.IndexFunc(in.Name, unicode.IsControl) >= 0 {
		return agentErr(http.StatusBadRequest, "name_invalid", "chat name must be 1 to 60 characters with no control characters")
	}
	if in.Model == "" {
		return agentErr(http.StatusBadRequest, "model_required", "a chat needs a model")
	}
	if len(in.Model) > maxChatTemplateModelBytes || strings.IndexFunc(in.Model, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return agentErr(http.StatusBadRequest, "model_invalid", "model must be at most 200 bytes with no whitespace or control characters")
	}
	return nil
}

// chatTemplateDigest binds the approval to the project, the name and the
// model, so an answer for one chat cannot be spent on another.
func chatTemplateDigest(projectID string, in ChatTemplateInput) presence.Digest {
	return presence.NewDigestBuilder("project.chat_template").
		StringField("project_id", true, projectID).
		StringField("action", true, "add").
		StringField("name", true, in.Name).
		StringField("model", true, in.Model).
		Build()
}

func chatTemplateReason(projectName string, in ChatTemplateInput) string {
	return fmt.Sprintf("Add chat %q to %s: model %s", in.Name, projectName, in.Model)
}

func (o *ChatTemplateOps) runCommitted(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.DoCommitted(ctx, func(context.Context) error { return fn() })
}

// Add appends a chat template after a presence approval.
func (o *ChatTemplateOps) Add(ctx context.Context, projectID string, in ChatTemplateInput, via, credID string) (res ChatTemplateAddResult, err error) {
	ev := logging.BeginEvent(ctx, "chat_template.add")
	defer func() {
		ev.Set("project_id", capAuditText(projectID, maxRefusalNameRunes)).Set("name", capAuditText(in.Name, maxChatTemplateNameRunes))
		endEvent(ev, agentEventErr(err))
	}()
	if rule := validateChatTemplateInput(in); rule != nil {
		return res, rule
	}
	p, _ := config.FindProjectByID(config.FreshSettings(o.Store), projectID)
	if p == nil {
		return res, agentErr(http.StatusNotFound, "project_not_found", "project not found")
	}
	if p.IsRemote() {
		return res, agentErr(http.StatusBadRequest, "project_not_available", "project is not available for chats")
	}
	snap := projectSnapshot(p)
	if rule := project.AgentNameClash(&snap, in.Name, ""); rule != nil {
		return res, agentRuleErr(rule)
	}
	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return res, err
	}
	grant, err := requireGate(o.Gate, ctx, "project.chat_template", chatTemplateDigest(projectID, in), chatTemplateReason(snap.Name, in),
		presenceAttempt{auditor: o.Issuance, via: via, credID: credID, subject: projectID})
	if err != nil {
		return res, err
	}

	template := config.ChatTemplate{ID: uuid.NewString(), Name: in.Name, Model: in.Model}
	var refusal error
	commitErr := o.runCommitted(ctx, func() error {
		saveErr := config.WithDeclinable(o.Store, func(s *config.Settings) error {
			// Subtle: the prompt was up while the record could move, so what the
			// approval covered is compared again here, inside the queue.
			live, _ := config.FindProjectByID(s, projectID)
			if live == nil || live.HostID != snap.HostID || live.Kind != snap.Kind || live.IsRemote() {
				refusal = agentErr(http.StatusConflict, "project_changed", "%s changed while the request was being approved; try again", snap.Name)
				return refusal
			}
			if rule := project.AgentNameClash(live, in.Name, ""); rule != nil {
				refusal = agentRuleErr(rule)
				return refusal
			}
			list := append(slices.Clone(live.ChatTemplates), template)
			s.UpdateProjectChatTemplates(projectID, list)
			iss := audit.CredentialIssuance{
				ConfigChange: true,
				Credential:   auditCredentialChatTemplate,
				Subject:      projectID,
				Name:         snap.Name,
				Grants:       []string{"action=add", "template=" + in.Name, "model=" + in.Model},
				Via:          via,
				CredID:       credID,
				PresenceID:   grant.ID(),
			}
			if auditErr := recordIssuance(o.Issuance, iss); auditErr != nil {
				slog.Error("chat template add declined: audit write failed", "project_id", projectID, "error", auditErr)
				refusal = agentUnrecordedError{}
				return refusal
			}
			return nil
		})
		if refusal != nil {
			return refusal
		}
		if saveErr != nil {
			return fmt.Errorf("%w: %w", errProjectSaveFailed, saveErr)
		}
		return nil
	})
	if commitErr != nil {
		return res, commitErr
	}
	return ChatTemplateAddResult{
		ProjectID: projectID,
		Template:  ChatTemplateView{ID: template.ID, Name: template.Name, Model: template.Model},
	}, nil
}
