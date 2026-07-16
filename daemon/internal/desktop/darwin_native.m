//go:build darwin && cgo

#import <Cocoa/Cocoa.h>
#import <Security/Security.h>
#import <ServiceManagement/ServiceManagement.h>

#include <stdlib.h>
#include <string.h>

#include "darwin_native.h"

static const NSUInteger codesk_maximum_secret_length = 64 * 1024;

static void codesk_set_error(char **error_out, NSString *message) {
    if (error_out == NULL) {
        return;
    }
    if (*error_out != NULL) {
        free(*error_out);
        *error_out = NULL;
    }
    const char *utf8 = message.UTF8String;
    *error_out = strdup(utf8 != NULL ? utf8 : "native macOS operation failed");
}

static NSString *codesk_security_error(NSString *operation, OSStatus status) {
    CFStringRef copied = SecCopyErrorMessageString(status, NULL);
    NSString *detail = CFBridgingRelease(copied);
    if (detail.length == 0) {
        detail = [NSString stringWithFormat:@"OSStatus %d", (int)status];
    }
    return [NSString stringWithFormat:@"%@: %@", operation, detail];
}

static NSMutableDictionary *codesk_keychain_query(NSString *service,
                                                   NSString *account,
                                                   BOOL forbid_ui) {
    // Omitting data-protection and access-group attributes intentionally selects
    // the per-user login Keychain for profile-less desktop builds.
    NSMutableDictionary *query = [@{
        (__bridge id)kSecClass: (__bridge id)kSecClassGenericPassword,
        (__bridge id)kSecAttrService: service,
        (__bridge id)kSecAttrAccount: account,
    } mutableCopy];
    if (forbid_ui) {
        query[(__bridge id)kSecUseAuthenticationUI] = (__bridge id)kSecUseAuthenticationUIFail;
    }
    return query;
}

static BOOL codesk_keychain_identity(const char *service_value,
                                     const char *account_value,
                                     NSString **service_out,
                                     NSString **account_out,
                                     char **error_out) {
    if (service_value == NULL || account_value == NULL) {
        codesk_set_error(error_out, @"invalid Keychain identity");
        return NO;
    }
    NSString *service = [NSString stringWithUTF8String:service_value];
    NSString *account = [NSString stringWithUTF8String:account_value];
    if (service.length == 0 || account.length == 0) {
        codesk_set_error(error_out, @"invalid Keychain identity");
        return NO;
    }
    *service_out = service;
    *account_out = account;
    return YES;
}

int codesk_keychain_save(const char *service_value, const char *account_value,
                         const void *secret, size_t secret_length,
                         char **error_out) {
    @autoreleasepool {
        NSString *service = nil;
        NSString *account = nil;
        if (!codesk_keychain_identity(service_value, account_value, &service, &account, error_out)) {
            return CODESK_NATIVE_ERROR;
        }
        if (secret == NULL || secret_length == 0) {
            codesk_set_error(error_out, @"invalid Keychain secret");
            return CODESK_NATIVE_ERROR;
        }
        NSData *data = [NSData dataWithBytesNoCopy:(void *)secret
                                             length:secret_length
                                       freeWhenDone:NO];
        NSDictionary *updates = @{
            (__bridge id)kSecValueData: data,
        };
        NSMutableDictionary *query = codesk_keychain_query(service, account, YES);
        OSStatus status = SecItemUpdate((__bridge CFDictionaryRef)query,
                                        (__bridge CFDictionaryRef)updates);
        if (status == errSecItemNotFound) {
            NSMutableDictionary *attributes = codesk_keychain_query(service, account, NO);
            [attributes addEntriesFromDictionary:updates];
            status = SecItemAdd((__bridge CFDictionaryRef)attributes, NULL);
            if (status == errSecDuplicateItem) {
                status = SecItemUpdate((__bridge CFDictionaryRef)query,
                                       (__bridge CFDictionaryRef)updates);
            }
        }
        if (status != errSecSuccess) {
            codesk_set_error(error_out, codesk_security_error(@"save Keychain credential", status));
            return CODESK_NATIVE_ERROR;
        }
        return CODESK_NATIVE_OK;
    }
}

int codesk_keychain_load(const char *service_value, const char *account_value,
                         void **secret_out, size_t *secret_length_out,
                         char **error_out) {
    @autoreleasepool {
        if (secret_out == NULL || secret_length_out == NULL) {
            codesk_set_error(error_out, @"invalid Keychain output");
            return CODESK_NATIVE_ERROR;
        }
        *secret_out = NULL;
        *secret_length_out = 0;
        NSString *service = nil;
        NSString *account = nil;
        if (!codesk_keychain_identity(service_value, account_value, &service, &account, error_out)) {
            return CODESK_NATIVE_ERROR;
        }
        NSMutableDictionary *query = codesk_keychain_query(service, account, YES);
        query[(__bridge id)kSecReturnData] = @YES;
        query[(__bridge id)kSecMatchLimit] = (__bridge id)kSecMatchLimitOne;
        CFTypeRef result = NULL;
        OSStatus status = SecItemCopyMatching((__bridge CFDictionaryRef)query, &result);
        if (status == errSecItemNotFound) {
            return CODESK_NATIVE_NOT_FOUND;
        }
        if (status != errSecSuccess) {
            codesk_set_error(error_out, codesk_security_error(@"load Keychain credential", status));
            return CODESK_NATIVE_ERROR;
        }
        NSData *data = CFBridgingRelease(result);
        if (![data isKindOfClass:[NSData class]] || data.length == 0 ||
            data.length > codesk_maximum_secret_length) {
            codesk_set_error(error_out, @"Keychain returned an invalid credential");
            return CODESK_NATIVE_ERROR;
        }
        void *copy = malloc(data.length);
        if (copy == NULL) {
            codesk_set_error(error_out, @"allocate Keychain credential copy");
            return CODESK_NATIVE_ERROR;
        }
        memcpy(copy, data.bytes, data.length);
        *secret_out = copy;
        *secret_length_out = data.length;
        return CODESK_NATIVE_OK;
    }
}

int codesk_keychain_delete(const char *service_value, const char *account_value,
                           char **error_out) {
    @autoreleasepool {
        NSString *service = nil;
        NSString *account = nil;
        if (!codesk_keychain_identity(service_value, account_value, &service, &account, error_out)) {
            return CODESK_NATIVE_ERROR;
        }
        NSMutableDictionary *query = codesk_keychain_query(service, account, YES);
        OSStatus status = SecItemDelete((__bridge CFDictionaryRef)query);
        if (status == errSecSuccess || status == errSecItemNotFound) {
            return CODESK_NATIVE_OK;
        }
        codesk_set_error(error_out, codesk_security_error(@"delete Keychain credential", status));
        return CODESK_NATIVE_ERROR;
    }
}

static void codesk_set_nserror(char **error_out, NSString *operation, NSError *error) {
    NSString *detail = error.localizedDescription;
    if (detail.length == 0) {
        detail = @"native macOS operation failed";
    }
    codesk_set_error(error_out, [NSString stringWithFormat:@"%@: %@", operation, detail]);
}

int codesk_login_item_enable(char **error_out) {
    @autoreleasepool {
        if (@available(macOS 13.0, *)) {
            SMAppService *service = [SMAppService mainAppService];
            if (service.status == SMAppServiceStatusEnabled) {
                return CODESK_NATIVE_OK;
            }
            NSError *error = nil;
            BOOL registered = [service registerAndReturnError:&error];
            if (service.status == SMAppServiceStatusEnabled) {
                return CODESK_NATIVE_OK;
            }
            if (service.status == SMAppServiceStatusRequiresApproval) {
                codesk_set_error(error_out, @"launch at login requires approval in System Settings > General > Login Items");
                return CODESK_NATIVE_REQUIRES_APPROVAL;
            }
            if (!registered) {
                codesk_set_nserror(error_out, @"enable launch at login", error);
                return CODESK_NATIVE_ERROR;
            }
            codesk_set_error(error_out, @"launch at login did not become enabled");
            return CODESK_NATIVE_ERROR;
        }
        codesk_set_error(error_out, @"launch at login requires macOS 13 or later");
        return CODESK_NATIVE_ERROR;
    }
}

int codesk_login_item_disable(char **error_out) {
    @autoreleasepool {
        if (@available(macOS 13.0, *)) {
            SMAppService *service = [SMAppService mainAppService];
            if (service.status == SMAppServiceStatusNotRegistered ||
                service.status == SMAppServiceStatusNotFound) {
                return CODESK_NATIVE_OK;
            }
            NSError *error = nil;
            BOOL unregistered = [service unregisterAndReturnError:&error];
            if (service.status == SMAppServiceStatusNotRegistered ||
                service.status == SMAppServiceStatusNotFound) {
                return CODESK_NATIVE_OK;
            }
            if (!unregistered) {
                codesk_set_nserror(error_out, @"disable launch at login", error);
                return CODESK_NATIVE_ERROR;
            }
            codesk_set_error(error_out, @"launch at login remained registered");
            return CODESK_NATIVE_ERROR;
        }
        codesk_set_error(error_out, @"launch at login requires macOS 13 or later");
        return CODESK_NATIVE_ERROR;
    }
}

int codesk_login_item_is_enabled(int *enabled_out, char **error_out) {
    @autoreleasepool {
        if (enabled_out == NULL) {
            codesk_set_error(error_out, @"invalid launch-at-login output");
            return CODESK_NATIVE_ERROR;
        }
        if (@available(macOS 13.0, *)) {
            *enabled_out = [SMAppService mainAppService].status == SMAppServiceStatusEnabled ? 1 : 0;
            return CODESK_NATIVE_OK;
        }
        codesk_set_error(error_out, @"launch at login requires macOS 13 or later");
        return CODESK_NATIVE_ERROR;
    }
}

int codesk_workspace_open(const char *target_value, int is_directory,
                          char **error_out) {
    @autoreleasepool {
        if (target_value == NULL) {
            codesk_set_error(error_out, @"invalid open target");
            return CODESK_NATIVE_ERROR;
        }
        NSString *target = [NSString stringWithUTF8String:target_value];
        if (target.length == 0) {
            codesk_set_error(error_out, @"invalid open target");
            return CODESK_NATIVE_ERROR;
        }
        NSURL *url = is_directory ? [NSURL fileURLWithPath:target isDirectory:YES]
                                  : [NSURL URLWithString:target];
        if (url == nil || ![[NSWorkspace sharedWorkspace] openURL:url]) {
            codesk_set_error(error_out, @"NSWorkspace could not open the target");
            return CODESK_NATIVE_ERROR;
        }
        return CODESK_NATIVE_OK;
    }
}

void codesk_show_fatal_error(const char *message_value) {
    @autoreleasepool {
        NSString *message = message_value != NULL ? [NSString stringWithUTF8String:message_value] : nil;
        if (message.length == 0) {
            message = @"Codesk could not start.";
        }
        void (^present)(void) = ^{
            [NSApplication sharedApplication];
            [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
            [NSApp activateIgnoringOtherApps:YES];
            NSAlert *alert = [[NSAlert alloc] init];
            alert.alertStyle = NSAlertStyleCritical;
            alert.messageText = @"Codesk could not start";
            alert.informativeText = message;
            [alert addButtonWithTitle:@"OK"];
            [alert runModal];
        };
        if ([NSThread isMainThread]) {
            present();
        } else {
            dispatch_sync(dispatch_get_main_queue(), present);
        }
    }
}

void codesk_error_free(char *error_message) {
    free(error_message);
}

void codesk_secret_free(void *secret, size_t secret_length) {
    if (secret == NULL) {
        return;
    }
    volatile unsigned char *bytes = (volatile unsigned char *)secret;
    for (size_t index = 0; index < secret_length; index++) {
        bytes[index] = 0;
    }
    free(secret);
}
