#include <sodium.h>
#include <stdio.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

static double seconds(void) {
    struct timespec now;
    if (clock_gettime(CLOCK_MONOTONIC, &now) != 0) return -1;
    return (double) now.tv_sec + (double) now.tv_nsec / 1000000000.0;
}

int main(void) {
    char password[256] = {0}, line[4096] = {0};
    char hash[crypto_pwhash_STRBYTES] = {0};
    unsigned int version, memory, iterations, lanes;
    int found = 0;
    FILE *users;
    alarm(20);
    if (fgets(password, sizeof password, stdin) == NULL) return 1;
    password[strcspn(password, "\r\n")] = 0;
    users = fopen("/data/users.txt", "r");
    if (users == NULL) return 2;
    while (fgets(line, sizeof line, users) != NULL) {
        char *end;
        if (strncmp(line, "normal:", 7) != 0) continue;
        end = strchr(line + 7, ':');
        if (end == NULL || (size_t) (end - line - 7) >= sizeof hash) return 3;
        memcpy(hash, line + 7, (size_t) (end - line - 7));
        found = 1;
        break;
    }
    fclose(users);
    if (!found) return 4;
    if (sscanf(hash, "$argon2id$v=%u$m=%u,t=%u,p=%u$", &version, &memory, &iterations, &lanes) != 4) {
        puts("S1 auth diagnostic unsupported_hash_metadata");
        return 5;
    }
    printf("S1 auth diagnostic algorithm=argon2id version=%u memory_kib=%u iterations=%u lanes=%u\n", version, memory, iterations, lanes);
    fflush(stdout);
    double start = seconds();
    int before = crypto_pwhash_str_verify(hash, password, strlen(password));
    printf("S1 auth diagnostic sodium_initialized=false password_valid=%d elapsed_seconds=%.6f\n", before == 0, seconds() - start);
    fflush(stdout);
    if (sodium_init() < 0) return 6;
    start = seconds();
    int after = crypto_pwhash_str_verify(hash, password, strlen(password));
    printf("S1 auth diagnostic sodium_initialized=true password_valid=%d elapsed_seconds=%.6f\n", after == 0, seconds() - start);
    sodium_memzero(password, sizeof password);
    sodium_memzero(line, sizeof line);
    sodium_memzero(hash, sizeof hash);
    return before == 0 && after == 0 ? 0 : 7;
}
