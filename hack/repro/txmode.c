/* SPDX-License-Identifier: Apache-2.0
 * Copyright 2026 Gluesys Co., Ltd.
 *
 * daos-operator#28: which operation inside an explicit DAOS transaction kills
 * the 2.8 engine?  put/remove x conditional/unconditional.
 *   txmode <system> <pool> <cont> <key> <mode>
 *     1 = tx + kv_put   (flags 0)
 *     2 = tx + kv_put   (DAOS_COND_KEY_INSERT)
 *     3 = tx + kv_remove(flags 0)
 *     4 = tx + kv_remove(DAOS_COND_KEY_REMOVE)
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <daos.h>
#include <daos_kv.h>

#define SAY(fmt, ...) do { printf(fmt "\n", ##__VA_ARGS__); fflush(stdout); } while (0)

int main(int argc, char **argv)
{
	daos_handle_t poh, coh, oh, tx;
	daos_obj_id_t oid = { .hi = 0x7667772d6c6f636bULL, .lo = 0x7075626c69636174ULL };
	const char *val = "v";
	int mode, rc;

	if (argc != 6) { fprintf(stderr, "usage: %s <system> <pool> <cont> <key> <mode>\n", argv[0]); return 2; }
	mode = atoi(argv[5]);
	if (daos_init()) return 1;
	if (daos_pool_connect(argv[2], argv[1], DAOS_PC_RW, &poh, NULL, NULL)) return 1;
	if (daos_cont_open(poh, argv[3], DAOS_COO_RW, &coh, NULL, NULL)) return 1;
	if (daos_obj_generate_oid(coh, &oid, DAOS_OT_KV_HASHED, 0, 0, 0)) return 1;
	if (daos_kv_open(coh, oid, DAOS_OO_RW, &oh, NULL)) return 1;

	/* remove modes need the key to exist; put modes need it gone */
	if (mode >= 3)
		daos_kv_put(oh, DAOS_TX_NONE, 0, argv[4], strlen(val), val, NULL);
	else
		daos_kv_remove(oh, DAOS_TX_NONE, 0, argv[4], NULL);

	rc = daos_tx_open(coh, &tx, 0, NULL);
	SAY("tx_open rc=%d", rc);
	if (rc) return 1;

	switch (mode) {
	case 1: SAY("mode 1: kv_put flags=0 in tx");
		rc = daos_kv_put(oh, tx, 0, argv[4], strlen(val), val, NULL); break;
	case 2: SAY("mode 2: kv_put DAOS_COND_KEY_INSERT in tx");
		rc = daos_kv_put(oh, tx, DAOS_COND_KEY_INSERT, argv[4], strlen(val), val, NULL); break;
	case 3: SAY("mode 3: kv_remove flags=0 in tx");
		rc = daos_kv_remove(oh, tx, 0, argv[4], NULL); break;
	default: SAY("mode 4: kv_remove DAOS_COND_KEY_REMOVE in tx");
		rc = daos_kv_remove(oh, tx, DAOS_COND_KEY_REMOVE, argv[4], NULL); break;
	}
	SAY("   -> op rc=%d", rc);
	if (rc == 0) { rc = daos_tx_commit(tx, NULL); SAY("   -> commit rc=%d", rc); }
	else daos_tx_abort(tx, NULL);
	daos_tx_close(tx, NULL);
	SAY("MODE %d SURVIVED", mode);
	daos_kv_close(oh, NULL); daos_cont_close(coh, NULL);
	daos_pool_disconnect(poh, NULL); daos_fini();
	return 0;
}
