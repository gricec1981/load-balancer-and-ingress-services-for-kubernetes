-- header_filter.lua
-- We may rewrite/truncate the body, so any Content-Length is invalid.
ngx.header.content_length = nil
ngx.header["X-AKO-AI-Shim"] = "active"
