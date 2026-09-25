package main

import "encoding/json"

// ipcListModels runs off the main thread: List makes a round trip to
// relay-sessions, which itself may spawn pi and query the model broker.
func ipcListModels(ctx *IPCContext, _ json.RawMessage) {
	ctx.GoFunc(func() {
		dispatchEmit(ctx, "onModelsListed", marshalForUI(ctx.ModelCatalog.List(ctx.Ctx)))
	})
}
