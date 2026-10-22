package domain

import "github.com/KDZZZZZZ/human-worth/backend/internal/contract"

type Operation = contract.OperationID

const (
	RequestTaskReview        = contract.RequestTaskReview
	ListReviews              = contract.ListReviews
	GetReview                = contract.GetReview
	DecideReview             = contract.DecideReview
	DownloadReviewAsset      = contract.DownloadReviewAsset
	CheckReviewAssetAccess   = contract.CheckReviewAssetAccess
	CreateDraft              = contract.CreateDraft
	ReadOwnSubmission        = contract.ReadOwnSubmission
	ListOwnSubmissions       = contract.ListOwnSubmissions
	ReplaceDraft             = contract.ReplaceDraft
	CurrentSession           = contract.CurrentSession
	Logout                   = contract.Logout
	CreateMCPToken           = contract.CreateMCPToken
	ListMCPTokens            = contract.ListMCPTokens
	RevokeMCPToken           = contract.RevokeMCPToken
	ReadTask                 = contract.ReadTask
	ListTasks                = contract.ListTasks
	ViewStatistics           = contract.ViewStatistics
	CastVote                 = contract.CastVote
	StartRun                 = contract.StartRun
	GetRun                   = contract.GetRun
	ListRuns                 = contract.ListRuns
	GetRunSummary            = contract.GetRunSummary
	CancelRun                = contract.CancelRun
	RestartRun               = contract.RestartRun
	RegisterCandidate        = contract.RegisterCandidate
	CreateAssetUploadSession = contract.CreateAssetUploadSession
	UploadAsset              = contract.UploadAsset
	CompleteAssetUpload      = contract.CompleteAssetUpload
	AbortAssetUpload         = contract.AbortAssetUpload
	GetAssetUploadSession    = contract.GetAssetUploadSession
	GetAssetMetadata         = contract.GetAssetMetadata
	ListMyAssets             = contract.ListMyAssets
	CheckAssetOwnership      = contract.CheckAssetOwnership
	DownloadAsset            = contract.DownloadAsset
)

// 【阅读 1】Policy 约束一个业务操作的受众与凭据用途；Write 表示存在副作用，web 调用需要 CSRF。
// Anonymous、MCP、Admin 分别控制匿名、MCP 只读凭据和管理员能力，不能由 HTTP 请求自行指定。
type Policy struct {
	Audience                     string
	Write, Anonymous, MCP, Admin bool
}

// 【阅读 2】PolicyFor 是 ResolvePrincipal 与 verify 共用的业务策略；transport/grpc/targets.go 绑定完整 RPC 名，未登记的方法默认拒绝。
// 表内未来模块的方法仅定义身份策略，不代表接口已经实现或公开；例如查看统计虽属读取入口，
// 仍有永久关闭投票资格的副作用，故标为 Write，实际资格事务应由 Voting 自己完成。
func PolicyFor(op Operation) (Policy, bool) {
	definition, ok := contract.LookupOperation(op)
	if !ok || !definition.HasPolicy {
		return Policy{}, false
	}
	static := definition.Policy
	return Policy{Audience: static.Audience, Write: static.Write, Anonymous: static.Anonymous, MCP: static.MCP, Admin: static.Admin}, true
}
func (p Policy) Authorize(actor Principal) error {
	if err := actor.Validate(); err != nil {
		return err
	}
	if actor.ClientKind == Anonymous && !p.Anonymous {
		return ErrUnauthenticated
	}
	if actor.ClientKind == MCPRead && !p.MCP {
		return ErrCredentialPurpose
	}
	if p.Admin && actor.Role != Admin {
		return ErrAdministratorRequired
	}
	return nil
}
