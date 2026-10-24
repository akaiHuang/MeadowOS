#ifndef MEADOW_SECURITY_BROKER_CLIENT_H
#define MEADOW_SECURITY_BROKER_CLIENT_H

#include <stddef.h>
#include <stdint.h>

#define MEADOW_PRODUCT_UPDATE_STATE_DIGEST_BYTES 32u
#define MEADOW_PRODUCT_UPDATE_STATE_MAX_BYTES 65536u

enum {
    MEADOW_SECURITY_BROKER_RESULT_OK = 0,
    MEADOW_SECURITY_BROKER_RESULT_FAILED = 1,
    MEADOW_SECURITY_BROKER_RESULT_CONFLICT = 2,
    MEADOW_SECURITY_BROKER_RESULT_REPLAY = 3,
};

typedef struct MeadowSecurityBrokerProductUpdateState {
    uint64_t version;
    unsigned char digest[MEADOW_PRODUCT_UPDATE_STATE_DIGEST_BYTES];
    unsigned char *blob;
    size_t blob_length;
} MeadowSecurityBrokerProductUpdateState;

typedef struct MeadowSecurityBrokerModelCatalogState {
    uint64_t version;
    uint64_t sequence;
    unsigned char digest[MEADOW_PRODUCT_UPDATE_STATE_DIGEST_BYTES];
} MeadowSecurityBrokerModelCatalogState;

char *MeadowSecurityBrokerCopySecret(
    const char *service_name,
    const char *purpose,
    char **error_out
);

int MeadowSecurityBrokerCopyProductUpdateState(
    const char *service_name,
    MeadowSecurityBrokerProductUpdateState *state_out,
    char **error_out
);

int MeadowSecurityBrokerCompareAndSwapProductUpdateState(
    const char *service_name,
    uint64_t expected_version,
    const unsigned char expected_digest[MEADOW_PRODUCT_UPDATE_STATE_DIGEST_BYTES],
    const unsigned char *replacement,
    size_t replacement_length,
    MeadowSecurityBrokerProductUpdateState *state_out,
    char **error_out
);

int MeadowSecurityBrokerCopyModelCatalogState(
    const char *service_name,
    MeadowSecurityBrokerModelCatalogState *state_out,
    char **error_out
);

int MeadowSecurityBrokerAdvanceModelCatalogState(
    const char *service_name,
    uint64_t sequence,
    const unsigned char digest[MEADOW_PRODUCT_UPDATE_STATE_DIGEST_BYTES],
    MeadowSecurityBrokerModelCatalogState *state_out,
    char **error_out
);

void MeadowSecurityBrokerReleaseProductUpdateState(
    MeadowSecurityBrokerProductUpdateState *state
);

void MeadowSecurityBrokerSecureZeroAndFree(void *memory, size_t length);

typedef struct MeadowSecurityBrokerModelVaultBindingV1 {
    const char *model_id;
    const char *version;
    const uint8_t *profile_digest;
    uint64_t profile_revision;
    uint32_t backend;
    const uint8_t *manifest_digest;
    uint32_t ordinal;
    const char *asset_id;
    uint32_t role;
    uint64_t plaintext_bytes;
    const uint8_t *plaintext_digest;
} MeadowSecurityBrokerModelVaultBindingV1;

void *MeadowSecurityBrokerModelVaultClientCreate(const char *service_name);
void MeadowSecurityBrokerModelVaultClientDestroy(void *client);
int MeadowSecurityBrokerModelVaultDestroyProductKeys(
    void *client,
    const char *model_id,
    const char *version,
    const uint8_t profile_digest[MEADOW_PRODUCT_UPDATE_STATE_DIGEST_BYTES],
    const uint8_t manifest_digest[MEADOW_PRODUCT_UPDATE_STATE_DIGEST_BYTES]);
int MeadowSecurityBrokerModelVaultBeginSeal(
    void *client,
    const MeadowSecurityBrokerModelVaultBindingV1 *binding,
    uint8_t **header,
    size_t *header_length,
    uint64_t *catalog_sequence,
    uint8_t catalog_digest[MEADOW_PRODUCT_UPDATE_STATE_DIGEST_BYTES]);
int MeadowSecurityBrokerModelVaultSealChunk(
    void *client,
    const MeadowSecurityBrokerModelVaultBindingV1 *binding,
    const uint8_t *header,
    size_t header_length,
    uint64_t chunk_index,
    int final_chunk,
    const uint8_t *plaintext,
    size_t plaintext_length,
    uint8_t **sealed,
    size_t *sealed_length);
int MeadowSecurityBrokerModelVaultOpenChunk(
    void *client,
    const MeadowSecurityBrokerModelVaultBindingV1 *binding,
    const uint8_t *header,
    size_t header_length,
    uint64_t chunk_index,
    int final_chunk,
    const uint8_t *sealed,
    size_t sealed_length,
    uint8_t **plaintext,
    size_t *plaintext_length);

#endif
