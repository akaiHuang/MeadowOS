package tools

import "context"

func collectSystemInfoPlatform(ctx context.Context, p systemInfoProbe, r *systemInfoReport) {
	collectSystemInfoLinux(ctx, p, r)
}
