#ifndef CODESK_DESKTOP_DARWIN_NATIVE_H
#define CODESK_DESKTOP_DARWIN_NATIVE_H

#include <stddef.h>

enum {
    CODESK_NATIVE_OK = 0,
    CODESK_NATIVE_NOT_FOUND = 1,
    CODESK_NATIVE_ERROR = 2,
    CODESK_NATIVE_REQUIRES_APPROVAL = 3,
};

int codesk_keychain_save(const char *service, const char *account,
                         const void *secret, size_t secret_length,
                         char **error_out);
int codesk_keychain_load(const char *service, const char *account,
                         void **secret_out, size_t *secret_length_out,
                         char **error_out);
int codesk_keychain_delete(const char *service, const char *account,
                           char **error_out);

int codesk_login_item_enable(char **error_out);
int codesk_login_item_disable(char **error_out);
int codesk_login_item_is_enabled(int *enabled_out, char **error_out);

int codesk_workspace_open(const char *target, int is_directory,
                          char **error_out);
void codesk_show_fatal_error(const char *message);

void codesk_error_free(char *error_message);
void codesk_secret_free(void *secret, size_t secret_length);

#endif
