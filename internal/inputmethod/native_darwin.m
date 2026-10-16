//go:build darwin && cgo

// matrix/hardened: scope native input-source changes to our terminal's GUI app.
#import <AppKit/AppKit.h>
#import <Carbon/Carbon.h>
#include <libproc.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include "native_darwin.h"

int deck_terminal_owner(void) {
    pid_t pid = getppid();
    for (int depth = 0; depth < 64 && pid > 1; depth++) {
        char path[PROC_PIDPATHINFO_MAXSIZE] = {0};
        if (proc_pidpath(pid, path, sizeof(path)) > 0 &&
            strstr(path, ".app/Contents/MacOS/")) {
            return pid;
        }
        struct proc_bsdinfo info;
        if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, sizeof(info)) != sizeof(info) ||
            info.pbi_ppid == pid) {
            break;
        }
        pid = info.pbi_ppid;
    }
    return 0;
}

int deck_is_foreground(int owner) {
    @autoreleasepool {
        return owner > 0 && [NSWorkspace sharedWorkspace].frontmostApplication.processIdentifier == owner;
    }
}

char *deck_source_id(int ascii) {
    TISInputSourceRef source = ascii ? TISCopyCurrentASCIICapableKeyboardInputSource()
                                     : TISCopyCurrentKeyboardInputSource();
    if (!source) return NULL;
    CFStringRef identifier = TISGetInputSourceProperty(source, kTISPropertyInputSourceID);
    char *result = NULL;
    if (identifier) {
        CFIndex capacity = CFStringGetMaximumSizeForEncoding(CFStringGetLength(identifier),
                                                            kCFStringEncodingUTF8) + 1;
        result = malloc(capacity);
        if (result && !CFStringGetCString(identifier, result, capacity, kCFStringEncodingUTF8)) {
            free(result);
            result = NULL;
        }
    }
    CFRelease(source);
    return result;
}

int deck_select_source(const char *identifier, int owner) {
    if (!identifier || !deck_is_foreground(owner)) return 0;
    CFStringRef key = CFStringCreateWithCString(NULL, identifier, kCFStringEncodingUTF8);
    if (!key) return 0;
    const void *keys[] = { kTISPropertyInputSourceID, kTISPropertyInputSourceIsEnabled };
    const void *values[] = { key, kCFBooleanTrue };
    CFDictionaryRef filter = CFDictionaryCreate(NULL, keys, values, 2,
                                               &kCFTypeDictionaryKeyCallBacks,
                                               &kCFTypeDictionaryValueCallBacks);
    CFArrayRef sources = TISCreateInputSourceList(filter, false);
    int success = 0;
    if (sources && CFArrayGetCount(sources) > 0 && deck_is_foreground(owner)) {
        success = TISSelectInputSource((TISInputSourceRef)CFArrayGetValueAtIndex(sources, 0)) == noErr;
    }
    if (sources) CFRelease(sources);
    CFRelease(filter);
    CFRelease(key);
    return success;
}
