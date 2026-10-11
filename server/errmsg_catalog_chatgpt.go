package server

// ChatGPT 구독 로그인(server/chatgpt.go) 계열 사용자 대면 메시지의 en/zh/es 번역.
// errmsg_catalog.go 는 생성 파일이라 건드리지 않고, 이 파일이 init 에서 같은 맵에 합친다.
// 키는 소스 상수의 한국어 원문 그대로여야 한다(번역 누락은 TestChatGPTMessagesTranslated 가 막는다).
func init() {
	for ko, tr := range map[string]map[string]string{
		errChatGPTInvalidState: {
			"en": "The state in the sign-in response does not match. Start the sign-in again from the beginning",
			"zh": "登录响应中的 state 不一致。请从头重新开始登录",
			"es": "El state de la respuesta de inicio de sesión no coincide. Vuelva a iniciar el proceso desde el principio",
		},
		errChatGPTDenied: {
			"en": "ChatGPT sign-in was cancelled or denied",
			"zh": "ChatGPT 登录已取消或被拒绝",
			"es": "El inicio de sesión de ChatGPT se canceló o fue denegado",
		},
		errChatGPTRegistration: {
			"en": "ChatGPT did not finish registering the app. Sign in again",
			"zh": "ChatGPT 未完成应用注册。请重新登录",
			"es": "ChatGPT no terminó de registrar la aplicación. Inicie sesión de nuevo",
		},
		errChatGPTInvalidGrant: {
			"en": "The authorization code has expired or was already used. Start the sign-in again",
			"zh": "授权码已过期或已被使用。请重新开始登录",
			"es": "El código de autorización ha caducado o ya se usó. Vuelva a iniciar el proceso de inicio de sesión",
		},
		errChatGPTInvalidIDToken: {
			"en": "Could not verify the ChatGPT account identity. Sign in again",
			"zh": "无法验证 ChatGPT 账户身份。请重新登录",
			"es": "No se pudo verificar la identidad de la cuenta de ChatGPT. Inicie sesión de nuevo",
		},
		errChatGPTIdentityDown: {
			"en": "The ChatGPT identity verification server is temporarily unreachable. Try again shortly",
			"zh": "暂时无法连接 ChatGPT 身份验证服务器，请稍后重试",
			"es": "El servidor de verificación de identidad de ChatGPT no está disponible temporalmente. Vuelva a intentarlo en breve",
		},
		errChatGPTAccountMismatch: {
			"en": "You signed in with a different ChatGPT account than before. Sign in with the original account or create a new profile",
			"zh": "登录的 ChatGPT 账户与之前不同。请使用原账户登录，或新建配置",
			"es": "Inició sesión con una cuenta de ChatGPT distinta de la anterior. Use la cuenta original o cree un perfil nuevo",
		},
		errChatGPTLoginRequired: {
			"en": "ChatGPT sign-in is required. Sign in again",
			"zh": "需要登录 ChatGPT。请重新登录",
			"es": "Se requiere iniciar sesión en ChatGPT. Inicie sesión de nuevo",
		},
		errChatGPTPlanDisabled: {
			"en": "Permission to use the ChatGPT plan was not granted. Sign in again and allow it",
			"zh": "未授予使用 ChatGPT 套餐的权限。请重新登录并允许该权限",
			"es": "No se concedió el permiso para usar el plan de ChatGPT. Inicie sesión de nuevo y permítalo",
		},
		errChatGPTNetwork: {
			"en": "Cannot reach the ChatGPT servers. Check the network and proxy settings",
			"zh": "无法连接 ChatGPT 服务器。请检查网络和代理设置",
			"es": "No se puede conectar con los servidores de ChatGPT. Compruebe la red y la configuración del proxy",
		},
		errChatGPTNotEligible: {
			"en": "Plan usage sharing is not available for this ChatGPT account or workspace",
			"zh": "此 ChatGPT 账户或工作区无法使用套餐用量共享",
			"es": "Compartir el uso del plan no está disponible para esta cuenta o espacio de trabajo de ChatGPT",
		},
		errChatGPTGeneric: {
			"en": "The ChatGPT request failed",
			"zh": "ChatGPT 请求失败",
			"es": "La solicitud a ChatGPT falló",
		},
		errChatGPTNoProfile: {
			"en": "No ChatGPT-subscription LLM profile was found",
			"zh": "未找到 ChatGPT 订阅方式的 LLM 配置",
			"es": "No se encontró ningún perfil de LLM con suscripción de ChatGPT",
		},
		errChatGPTNoPending: {
			"en": "There is no sign-in in progress. Start the sign-in again",
			"zh": "没有正在进行的 ChatGPT 登录。请重新开始登录",
			"es": "No hay ningún inicio de sesión en curso. Vuelva a iniciar el proceso",
		},
		errChatGPTBadCallback: {
			"en": "Could not read the sign-in details from the pasted address",
			"zh": "无法从粘贴的地址中读取登录信息",
			"es": "No se pudo leer la información de inicio de sesión de la dirección pegada",
		},
		errChatGPTTooManyLogins: {
			"en": "Too many sign-ins are in progress. Try again shortly",
			"zh": "正在进行的登录过多。请稍后重试",
			"es": "Hay demasiados inicios de sesión en curso. Vuelva a intentarlo en breve",
		},
		errMgmtLLMBadAuthMode: {
			"en": "Unsupported authentication method",
			"zh": "不支持的认证方式",
			"es": "Método de autenticación no admitido",
		},
		reasonLLMProfileChatGPTOff: {
			"en": "The ChatGPT account for this LLM profile is not signed in",
			"zh": "此 LLM 配置的 ChatGPT 账户未登录",
			"es": "La cuenta de ChatGPT de este perfil de LLM no ha iniciado sesión",
		},
		msgChatGPTCallbackDoneTitle: {
			"en": "ChatGPT sign-in is complete",
			"zh": "ChatGPT 登录已完成",
			"es": "El inicio de sesión de ChatGPT ha finalizado",
		},
		msgChatGPTCallbackDoneBody: {
			"en": "Close this tab and return to the ARTEX settings page.",
			"zh": "请关闭此标签页，返回 ARTEX 设置页面。",
			"es": "Cierre esta pestaña y vuelva a la pantalla de ajustes de ARTEX.",
		},
		msgChatGPTCallbackFailTitle: {
			"en": "Could not finish the ChatGPT sign-in",
			"zh": "未能完成 ChatGPT 登录",
			"es": "No se pudo completar el inicio de sesión de ChatGPT",
		},
		msgChatGPTCallbackBadReq: {
			"en": "This sign-in request is not valid. Start the sign-in again from the ARTEX settings page.",
			"zh": "此登录请求无效。请在 ARTEX 设置页面重新开始登录。",
			"es": "Esta solicitud de inicio de sesión no es válida. Vuelva a iniciar el proceso desde los ajustes de ARTEX.",
		},
	} {
		errCatalog[ko] = tr
	}
}
