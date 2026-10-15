package server

// claude-code 인증 방식(server/claudecode.go) 사용자 대면 메시지의 en/zh/es 번역.
// errmsg_catalog.go 는 생성 파일이라 건드리지 않고, 이 파일이 init 에서 같은 맵에 합친다.
// 키는 소스 상수의 한국어 원문 그대로여야 한다(번역 누락은 TestClaudeCodeMessagesTranslated 가 막는다).
func init() {
	for ko, tr := range map[string]map[string]string{
		errClaudeCodeAckRequired: {
			"en": "You must confirm the personal local use notice before enabling claude-code",
			"zh": "启用 claude-code 前必须确认个人本地使用声明",
			"es": "Debe confirmar el aviso de uso personal y local antes de activar claude-code",
		},
		errClaudeCodeDisabled: {
			"en": "The claude-code authentication method is turned off. Turn it on in the LLM settings first",
			"zh": "claude-code 认证方式已关闭。请先在 LLM 设置中开启",
			"es": "El método de autenticación claude-code está desactivado. Actívelo primero en los ajustes de LLM",
		},
		errClaudeCodeNotLoopback: {
			"en": "claude-code cannot be turned on because this ARTEX is reachable on an address other than loopback (127.0.0.1). Run it bound to 127.0.0.1 so only your own PC can reach it",
			"zh": "此 ARTEX 可通过 loopback（127.0.0.1）以外的地址访问，因此无法开启 claude-code。请绑定到 127.0.0.1 运行，使其只能从本机访问",
			"es": "No se puede activar claude-code porque este ARTEX es accesible desde una dirección distinta de loopback (127.0.0.1). Ejecútelo enlazado a 127.0.0.1 para que solo su propio PC pueda acceder",
		},
		errClaudeCodeBadEffort: {
			"en": "Reasoning effort must be one of low, medium, high, xhigh, max",
			"zh": "推理强度必须是 low、medium、high、xhigh、max 之一",
			"es": "El esfuerzo de razonamiento debe ser low, medium, high, xhigh o max",
		},
		errClaudeCodeBadBinary: {
			"en": "The claude executable path is not valid",
			"zh": "claude 可执行文件路径无效",
			"es": "La ruta del ejecutable de claude no es válida",
		},
		reasonLLMProfileClaudeCodeOff: {
			"en": "The claude-code authentication method is turned off for this LLM profile",
			"zh": "此 LLM 配置的 claude-code 认证方式已关闭",
			"es": "El método de autenticación claude-code está desactivado para este perfil de LLM",
		},
		msgClaudeCodeNotInstalled: {
			"en": "The claude executable was not found. Install Claude Code and sign in yourself in a terminal with `claude auth login`",
			"zh": "未找到 claude 可执行文件。请安装 Claude Code，并在终端中自行运行 `claude auth login` 登录",
			"es": "No se encontró el ejecutable de claude. Instale Claude Code e inicie sesión usted mismo en un terminal con `claude auth login`",
		},
		msgClaudeCodeTooOld: {
			"en": "The installed Claude Code version is too old. Update it with `claude update`",
			"zh": "已安装的 Claude Code 版本过低。请用 `claude update` 更新",
			"es": "La versión instalada de Claude Code es demasiado antigua. Actualícela con `claude update`",
		},
		msgClaudeCodeNotLoggedIn: {
			"en": "claude is not signed in. Run `claude auth login` yourself in a terminal and sign in with your own subscription",
			"zh": "claude 尚未登录。请在终端中自行运行 `claude auth login`，使用您本人的订阅登录",
			"es": "claude no ha iniciado sesión. Ejecute usted mismo `claude auth login` en un terminal e inicie sesión con su propia suscripción",
		},
		msgClaudeCodeAPIBilling: {
			"en": "claude is using a non-subscription authentication method (such as an API key), which would bill the API. Only subscription sign-in (claude.ai) is allowed",
			"zh": "claude 正在使用非订阅的认证方式（如 API Key），会产生 API 计费。只允许订阅登录（claude.ai）",
			"es": "claude usa un método de autenticación que no es la suscripción (como una clave de API), lo que facturaría la API. Solo se permite el inicio de sesión con suscripción (claude.ai)",
		},
		msgClaudeCodeUnknown: {
			"en": "Could not determine the state of claude. Run `claude auth status` yourself in a terminal",
			"zh": "无法确认 claude 的状态。请在终端中自行运行 `claude auth status`",
			"es": "No se pudo determinar el estado de claude. Ejecute usted mismo `claude auth status` en un terminal",
		},
	} {
		errCatalog[ko] = tr
	}
}
