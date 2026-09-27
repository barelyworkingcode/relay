#ifndef DEVBOXPRESENCE_DIALOG_DARWIN_H
#define DEVBOXPRESENCE_DIALOG_DARWIN_H

#include <stdint.h>

typedef struct {
	uint32_t window;
	int32_t pid;
} dbp_window;

typedef struct {
	int on_console;
	int locked;
	int ax_trusted;
	int post_events;
	int window_list;
} dbp_session;

int dbp_list_windows(dbp_window *out, int max);
int dbp_owner_ok(int32_t pid, char *detail, int detail_len);
int dbp_window_text(int32_t pid, uint32_t window, char *buf, int buf_len);
void dbp_probe_session(dbp_session *st);
int dbp_answer(int32_t pid, uint32_t window, const char *expect, const uint16_t *pw, int n, char *detail, int detail_len);
int dbp_cancel(int32_t pid, uint32_t window, char *detail, int detail_len);

#endif
