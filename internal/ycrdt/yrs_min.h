#ifndef NOTTY_YRS_MIN_H
#define NOTTY_YRS_MIN_H

#include <stdint.h>

#define Y_OFFSET_BYTES 0
#define Y_OFFSET_UTF16 1
#define Y_JSON_STR -5
#define Y_MAP 2

typedef struct YDoc YDoc;
typedef struct YTransaction YTransaction;
typedef struct Branch Branch;
typedef struct YStickyIndex YStickyIndex;
typedef struct YMapIter YMapIter;

typedef union YOutputContent {
  uint8_t flag;
  double num;
  int64_t integer;
  char *str;
  const char *buf;
  struct YOutput *array;
  struct YMapEntry *map;
  Branch *y_type;
  YDoc *y_doc;
} YOutputContent;

typedef struct YOutput {
  int8_t tag;
  uint32_t len;
  union YOutputContent value;
} YOutput;

typedef struct YMapEntry {
  const char *key;
  const struct YOutput *value;
} YMapEntry;

typedef struct YMapInputData {
  char **keys;
  struct YInput *values;
} YMapInputData;

typedef union YInputContent {
  uint8_t flag;
  double num;
  int64_t integer;
  char *str;
  char *buf;
  struct YInput *values;
  struct YMapInputData map;
  YDoc *doc;
  const void *weak;
} YInputContent;

typedef struct YInput {
  int8_t tag;
  uint32_t len;
  union YInputContent value;
} YInput;

typedef struct YOptions {
  uint64_t id;
  const char *guid;
  const char *collection_id;
  uint8_t encoding;
  uint8_t skip_gc;
  uint8_t auto_load;
  uint8_t should_load;
} YOptions;

YOptions yoptions(void);

void ybinary_destroy(char *ptr, uint32_t len);
void ystring_destroy(char *str);
void youtput_destroy(struct YOutput *val);

YDoc *ydoc_new_with_options(YOptions options);
void ydoc_destroy(YDoc *value);
uint64_t ydoc_id(YDoc *doc);

YTransaction *ydoc_read_transaction(YDoc *doc);
YTransaction *ydoc_write_transaction(YDoc *doc, uint32_t origin_len, const char *origin);
void ytransaction_commit(YTransaction *txn);
char *ytransaction_state_vector_v1(const YTransaction *txn, uint32_t *len);
char *ytransaction_state_diff_v1(const YTransaction *txn, const char *sv, uint32_t sv_len, uint32_t *len);
uint8_t ytransaction_apply(YTransaction *txn, const char *diff, uint32_t diff_len);

Branch *ytext(YDoc *doc, const char *name);
uint32_t ytext_len(const Branch *txt, const YTransaction *txn);
char *ytext_string(const Branch *txt, const YTransaction *txn);
void ytext_insert(const Branch *txt, YTransaction *txn, uint32_t index, const char *value, const void *attrs);
void ytext_remove_range(const Branch *txt, YTransaction *txn, uint32_t index, uint32_t length);

Branch *ymap(YDoc *doc, const char *name);
YMapIter *ymap_iter(const Branch *map, const YTransaction *txn);
void ymap_iter_destroy(YMapIter *iter);
struct YMapEntry *ymap_iter_next(YMapIter *iter);
void ymap_entry_destroy(struct YMapEntry *value);
void ymap_insert(const Branch *map, YTransaction *txn, const char *key, const struct YInput *value);
uint8_t ymap_remove(const Branch *map, YTransaction *txn, const char *key);
struct YOutput *ymap_get(const Branch *map, const YTransaction *txn, const char *key);
struct YInput yinput_string(const char *str);
struct YInput yinput_ymap(char **keys, struct YInput *values, uint32_t len);
Branch *youtput_read_ymap(const struct YOutput *val);

YStickyIndex *ysticky_index_from_index(const Branch *branch, YTransaction *txn, uint32_t index, int8_t assoc);
char *ysticky_index_encode(const YStickyIndex *pos, uint32_t *len);
YStickyIndex *ysticky_index_decode(const char *binary, uint32_t len);
char *ysticky_index_to_json(const YStickyIndex *pos);
YStickyIndex *ysticky_index_from_json(const char *json);
void ysticky_index_read(const YStickyIndex *pos, const YTransaction *txn, Branch **out_branch, uint32_t *out_index);
void ysticky_index_destroy(YStickyIndex *pos);

#endif
