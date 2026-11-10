# TG-319: forward the OpenAI `user` field to the model backend.
#
# LiteLLM consumes the top-level `user` for its OWN per-key budget/rate accounting and does NOT pass it to a
# custom `api_base` upstream (the openai-compatible passthrough drops it; measured under drop_params true AND
# false, and with forward_client_headers_to_llm_api on — deploy/model_channel comments + TG-319). So every TG
# completion reached the tg-claude-proxy with `caller:""`, and per-caller cost/attribution at the proxy was
# blind.
#
# `extra_body`, by contrast, is forwarded into the upstream request body untouched. Proven live 2026-08-12:
# a probe carrying `extra_body:{user:X}` reached the sidecar as `caller:X`, while a top-level `user:Y` reached
# it as `caller:""`. So this pre-call hook copies `user` into `extra_body` — leaving the top-level `user` in
# place so LiteLLM's budgeting (which reads it BEFORE this payload is built) is unaffected. The proxy reads it
# from the request body (OaiChatRequest.user, deploy/claude-proxy/src/main.rs).
#
# It never blocks a call: any failure inside the hook is swallowed (attribution is best-effort; a model call
# must not fail because caller-tagging did).
from litellm.integrations.custom_logger import CustomLogger


class ForwardUser(CustomLogger):
    async def async_pre_call_hook(self, user_api_key_dict, cache, data, call_type):
        try:
            user = data.get("user")
            if user:
                extra = data.get("extra_body")
                if not isinstance(extra, dict):
                    extra = {}
                    data["extra_body"] = extra
                # Do not clobber a caller that already set extra_body.user explicitly.
                extra.setdefault("user", user)
        except Exception as e:
            # Best-effort: a model call must NEVER fail because caller-tagging failed. But do not fail
            # SILENTLY — if this hook starts raising, attribution reverts to caller:"" (the exact bug this
            # fixes), so leave a breadcrumb. The logger import is itself guarded so a litellm internal move
            # cannot turn this into a hard dependency (TG-319 review #2).
            try:
                from litellm._logging import verbose_proxy_logger

                verbose_proxy_logger.debug("tg319 forward_user: caller tagging skipped: %s", e)
            except Exception:
                pass
        return data


forward_user_instance = ForwardUser()
