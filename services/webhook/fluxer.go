package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	webhook_model "gitea.dev/models/webhook"
	"gitea.dev/modules/git"
	"gitea.dev/modules/json"
	"gitea.dev/modules/log"
	"gitea.dev/modules/setting"
	api "gitea.dev/modules/structs"
	"gitea.dev/modules/util"
	webhook_module "gitea.dev/modules/webhook"
)

type (
	FluxerEmbedFooter struct {
		Text    string `json:"text"`
		IconURL string `json:"icon_url"`
	}

	FluxerEmbedAuthor struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		IconURL string `json:"icon_url"`
	}

	FluxerEmbedField struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}

	FluxerEmbedMedia struct {
		URL         string `json:"url"`
		Description string `json:"description"`
	}

	FluxerEmbed struct {
		Title       string             `json:"title"`
		Description string             `json:"description"`
		URL         string             `json:"url"`
		Color       int                `json:"color"`
		Footer      FluxerEmbedFooter  `json:"footer"`
		Author      FluxerEmbedAuthor  `json:"author"`
		Fields      []FluxerEmbedField `json:"fields"`
	}

	FluxerPayload struct {
		Username  string        `json:"username,omitempty"`
		AvatarURL string        `json:"avatarURL,omitempty"`
		Content   string        `json:"content"`
		Embeds    []FluxerEmbed `json:"embeds"`
	}
	// FluxerMeta contains the fluxer metadata
	FluxerMeta struct {
		Username string `json:"username"`
		IconURL  string `json:"icon_url"`
	}
)

// GetFluxerHook returns fluxer metadata
func GetFluxerHook(w *webhook_model.Webhook) *FluxerMeta {
	s := &FluxerMeta{}
	if err := json.Unmarshal([]byte(w.Meta), s); err != nil {
		log.Error("webhook.GetFluxerHook(%d): %v", w.ID, err)
	}
	return s
}

// Limits from Fluxer EmbedBuilder.ts and MessagePayload.ts.
const (
	fluxerContentLimit     = 2000
	fluxerTitleLimit       = 256
	fluxerDescriptionLimit = 4096
	fluxerAuthorNameLimit  = 256
	fluxerFooterTextLimit  = 2048
	fluxerFieldNameLimit   = 256
	fluxerFieldValueLimit  = 1024
	fluxerMaxFields        = 25
	fluxerEmbedTotalLimit  = 6000
	fluxerEmbedsMax        = 10
)

type fluxerConvertor struct {
	Username  string
	AvatarURL string
}

func (f fluxerConvertor) Create(p *api.CreatePayload) (FluxerPayload, error) {
	refName := git.RefName(p.Ref).ShortName()
	title := fmt.Sprintf("[%s] %s %s created", p.Repo.FullName, p.RefType, refName)

	return f.createPayload(p.Sender, title, "", p.Repo.HTMLURL+"/src/"+util.PathEscapeSegments(refName), greenColor), nil
}

func (f fluxerConvertor) Delete(p *api.DeletePayload) (FluxerPayload, error) {
	refName := git.RefName(p.Ref).ShortName()
	title := fmt.Sprintf("[%s] %s %s deleted", p.Repo.FullName, p.RefType, refName)

	return f.createPayload(p.Sender, title, "", p.Repo.HTMLURL+"/src/"+util.PathEscapeSegments(refName), redColor), nil
}

func (f fluxerConvertor) Fork(p *api.ForkPayload) (FluxerPayload, error) {
	title := fmt.Sprintf("%s is forked to %s", p.Forkee.FullName, p.Repo.FullName)

	return f.createPayload(p.Sender, title, "", p.Repo.HTMLURL, greenColor), nil
}

func (f fluxerConvertor) Push(p *api.PushPayload) (FluxerPayload, error) {
	branchName := git.RefName(p.Ref).ShortName()
	var commitDesc string
	var titleLink string
	if p.TotalCommits == 1 {
		commitDesc = "1 new commit"
		titleLink = p.Commits[0].URL
	} else {
		commitDesc = fmt.Sprintf("%d new commits", p.TotalCommits)
		titleLink = p.CompareURL
	}
	if titleLink == "" {
		titleLink = p.Repo.HTMLURL + "/src/" + util.PathEscapeSegments(branchName)
	}
	title := fmt.Sprintf("[%s:%s] %s", p.Repo.FullName, branchName, commitDesc)

	var text strings.Builder
	for i, commit := range p.Commits {
		message := strings.TrimRight(strings.SplitN(commit.Message, "\n", 2)[0], "\r")
		if utf8.RuneCountInString(message) > 50 {
			message = fmt.Sprintf("%.47s...", message)
		}
		fmt.Fprintf(&text, "[%s](%s) %s - %s", commit.ID[:7], commit.URL, message, commit.Author.Name)
		if i < len(p.Commits)-1 {
			text.WriteString("\n")
		}
	}

	return f.createPayload(p.Sender, title, text.String(), titleLink, greenColor), nil
}

func (f fluxerConvertor) Issue(p *api.IssuePayload) (FluxerPayload, error) {
	title, _, extraMarkdown, color := getIssuesPayloadInfo(p, noneLinkFormatter, false)

	return f.createPayload(p.Sender, title, extraMarkdown, p.Issue.HTMLURL, color), nil
}

func (f fluxerConvertor) IssueComment(p *api.IssueCommentPayload) (FluxerPayload, error) {
	title, _, color := getIssueCommentPayloadInfo(p, noneLinkFormatter, false)

	return f.createPayload(p.Sender, title, p.Comment.Body, p.Comment.HTMLURL, color), nil
}

func (f fluxerConvertor) PullRequest(p *api.PullRequestPayload) (FluxerPayload, error) {
	title, _, extraMarkdown, color := getPullRequestPayloadInfo(p, noneLinkFormatter, false)

	return f.createPayload(p.Sender, title, extraMarkdown, p.PullRequest.HTMLURL, color), nil
}

func (f fluxerConvertor) Review(p *api.PullRequestPayload, event webhook_module.HookEventType) (FluxerPayload, error) {
	var text, title string
	var color int
	switch p.Action {
	case api.HookIssueReviewed:
		action, err := parseFluxerHookPullRequestEventType(event)
		if err != nil {
			return FluxerPayload{}, err
		}
		title = fmt.Sprintf("[%s] Pull request review %s: #%d %s", p.Repository.FullName, action, p.Index, p.PullRequest.Title)
		text = p.Review.Content
		switch event {
		case webhook_module.HookEventPullRequestReviewApproved:
			color = greenColor
		case webhook_module.HookEventPullRequestReviewRejected:
			color = redColor
		case webhook_module.HookEventPullRequestReviewComment:
			color = greyColor
		default:
			color = yellowColor
		}
	}

	return f.createPayload(p.Sender, title, text, p.PullRequest.HTMLURL, color), nil
}

func (f fluxerConvertor) Repository(p *api.RepositoryPayload) (FluxerPayload, error) {
	var title, htmlURL string
	var color int
	switch p.Action {
	case api.HookRepoCreated:
		title = fmt.Sprintf("[%s] Repository created", p.Repository.FullName)
		htmlURL = p.Repository.HTMLURL
		color = greenColor
	case api.HookRepoDeleted:
		title = fmt.Sprintf("[%s] Repository deleted", p.Repository.FullName)
		color = redColor
	case api.HookRepoRenamed:
		title = fmt.Sprintf("[%s] Repository renamed from %s", p.Repository.FullName, getRepoRenamedFrom(p))
		htmlURL = p.Repository.HTMLURL
		color = greenColor
	}

	return f.createPayload(p.Sender, title, "", htmlURL, color), nil
}

func (f fluxerConvertor) Wiki(p *api.WikiPayload) (FluxerPayload, error) {
	text, color, _ := getWikiPayloadInfo(p, noneLinkFormatter, false)
	htmlLink := p.Repository.HTMLURL + "/wiki/" + url.PathEscape(p.Page)
	var description string
	if p.Action != api.HookWikiDeleted {
		description = p.Comment
	}

	return f.createPayload(p.Sender, text, description, htmlLink, color), nil
}

func (f fluxerConvertor) Release(p *api.ReleasePayload) (FluxerPayload, error) {
	text, color := getReleasePayloadInfo(p, noneLinkFormatter, false)

	return f.createPayload(p.Sender, text, p.Release.Note, p.Release.HTMLURL, color), nil
}

func (f fluxerConvertor) Package(p *api.PackagePayload) (FluxerPayload, error) {
	text, color := getPackagePayloadInfo(p, noneLinkFormatter, false)

	return f.createPayload(p.Sender, text, "", p.Package.HTMLURL, color), nil
}

func (f fluxerConvertor) Status(p *api.CommitStatusPayload) (FluxerPayload, error) {
	text, color := getStatusPayloadInfo(p, noneLinkFormatter, false)

	return f.createPayload(p.Sender, text, "", p.TargetURL, color), nil
}

func (f fluxerConvertor) WorkflowRun(p *api.WorkflowRunPayload) (FluxerPayload, error) {
	text, color := getWorkflowRunPayloadInfo(p, noneLinkFormatter, false)
	return f.createPayload(p.Sender, text, "", p.WorkflowRun.HTMLURL, color), nil
}

func (f fluxerConvertor) WorkflowJob(p *api.WorkflowJobPayload) (FluxerPayload, error) {
	text, color := getWorkflowJobPayloadInfo(p, noneLinkFormatter, false)
	return f.createPayload(p.Sender, text, "", p.WorkflowJob.HTMLURL, color), nil
}

func newFluxerRequest(_ context.Context, w *webhook_model.Webhook, t *webhook_model.HookTask) (*http.Request, []byte, error) {
	meta := &FluxerMeta{}
	if err := json.Unmarshal([]byte(w.Meta), meta); err != nil {
		return nil, nil, fmt.Errorf("newFluxerRequest meta json: %w", err)
	}

	var pc payloadConvertor[FluxerPayload] = fluxerConvertor{
		Username:  meta.Username,
		AvatarURL: meta.IconURL,
	}
	return newJSONRequest(pc, w, t, true)
}

func init() {
	RegisterWebhookRequester(webhook_module.FLUXER, newFluxerRequest)
}

func parseFluxerHookPullRequestEventType(event webhook_module.HookEventType) (string, error) {
	switch event {
	case webhook_module.HookEventPullRequestReviewApproved:
		return "approved", nil
	case webhook_module.HookEventPullRequestReviewRejected:
		return "requested changes", nil
	case webhook_module.HookEventPullRequestReviewComment:
		return "comment", nil
	default:
		return "", errors.New("unknown event type")
	}
}

func (f fluxerConvertor) createPayload(s *api.User, title, text, link string, color int) FluxerPayload {
	embed := FluxerEmbed{
		Title:       util.TruncateRunes(title, fluxerTitleLimit),
		Description: util.TruncateRunes(text, fluxerDescriptionLimit),
		URL:         link,
		Color:       color,
		// Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Author: FluxerEmbedAuthor{
			Name:    util.TruncateRunes(s.UserName, fluxerAuthorNameLimit),
			URL:     setting.AppURL + s.UserName,
			IconURL: s.AvatarURL,
		},
		Footer: FluxerEmbedFooter{
			Text:    "Gitea",
			IconURL: s.AvatarURL,
		},
	}

	// Fluxer requires total embed character count <= 6000.
	total := len(embed.Title) + len(embed.Description) + len(embed.Author.Name) + len(embed.Footer.Text)
	for _, field := range embed.Fields {
		total += len(field.Name) + len(field.Value)
	}
	if total > fluxerEmbedTotalLimit {
		allowed := fluxerEmbedTotalLimit - (total - len(embed.Description))
		if allowed < 0 {
			allowed = 0
		}
		embed.Description = util.TruncateRunes(embed.Description, allowed)
	}

	return FluxerPayload{
		Username:  f.Username,
		AvatarURL: f.AvatarURL,
		Embeds:    []FluxerEmbed{embed},
	}
}
