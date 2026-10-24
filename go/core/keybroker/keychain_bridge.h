#ifndef MEADOW_KEYBROKER_KEYCHAIN_BRIDGE_H
#define MEADOW_KEYBROKER_KEYCHAIN_BRIDGE_H

#include <stddef.h>
#include <stdint.h>

#define MEADOW_KEYBROKER_STATUS_OK 0
#define MEADOW_KEYBROKER_STATUS_NOT_FOUND 1
#define MEADOW_KEYBROKER_STATUS_DUPLICATE 2
#define MEADOW_KEYBROKER_STATUS_FAILED 3
#define MEADOW_KEYBROKER_STATUS_INTERACTION_REQUIRED 4

int MeadowKeyBrokerCopy(
    const char *keychain_path,
    const char *service,
    const char *account,
    unsigned char **data_out,
    size_t *data_length_out,
    char **error_out
);

int MeadowKeyBrokerCopyLatest(
    const char *keychain_path,
    const char *service,
    const char *account_prefix,
    unsigned char **data_out,
    size_t *data_length_out,
    uint64_t *version_out,
    char **error_out
);

int MeadowKeyBrokerAdd(
    const char *keychain_path,
    const char *service,
    const char *account,
    const unsigned char *data,
    size_t data_length,
    char **error_out
);

void MeadowKeyBrokerFreeSecret(unsigned char *data, size_t data_length);

#endif
