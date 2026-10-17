// constants.go SOLO 上游技术常量。客户端版本须从官方发布与 product.json 核实，并验证协议兼容性。
package upstream

const (
	AgentHost   = "https://trae-api-cn.mchost.guru"
	UgHost      = "https://api.trae.cn"
	OAuthHost   = "https://api.trae.com.cn"
	ConsoleHost = "https://www.trae.cn"
	ClientID    = "en1oxy7wnw8j9n" // SOLO stable
	AppID       = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// TRAE CN 3.4.2 (build 2.3.91092), verified against the official latest
	// release endpoint and the installed client's product.json on 2026-10-11.
	IdeVersion = "3.4.2"
	// Keep the verified SOLO gateway protocol code independently of the app
	// version: product.json's traeVersionCode=20250325 returns an empty catalog;
	// the IDE extension's 20260212 returns only 15 configs, while 20260811
	// returns the full 46-config SOLO catalog with app version 3.4.2.
	IdeVersionCode = "20260811"
	DeviceBrand    = "Apple"
	OSVersion      = "macOS 15.7.4"
	Function       = "solo_work_lite"

	// 端点
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)
