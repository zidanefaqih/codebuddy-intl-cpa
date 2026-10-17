// Windows C-host fixture: exercise the real Go DLL and nested host callbacks.
#include <windows.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
typedef struct { void *ptr; size_t len; } buffer;
typedef struct { uint32_t version; void *ctx; int (*call)(void*,const char*,const uint8_t*,size_t,buffer*); void (*free_buffer)(void*,size_t); } host_api;
typedef struct { uint32_t version; int (*call)(char*,uint8_t*,size_t,buffer*); void (*free_buffer)(void*,size_t); void (*shutdown)(void); } plugin_api;
typedef int (*init_fn)(host_api*,plugin_api*);
static LONG closed=0;
static int codebuddy_stream=0;
static FILE *events;
static char *encoded(const char *s) {
  static const char table[]="ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  size_t n=strlen(s),j=0;char *out=malloc(4*((n+2)/3)+1);
  for(size_t i=0;i<n;i+=3){unsigned v=(unsigned char)s[i]<<16;if(i+1<n)v|=(unsigned char)s[i+1]<<8;if(i+2<n)v|=(unsigned char)s[i+2];out[j++]=table[v>>18];out[j++]=table[(v>>12)&63];out[j++]=i+1<n?table[(v>>6)&63]:'=';out[j++]=i+2<n?table[v&63]:'=';}out[j]=0;return out;
}
static void free_host(void *p,size_t n){(void)n;free(p);}
static int host_call(void *ctx,const char *m,const uint8_t *q,size_t n,buffer *out){
  (void)ctx;const char *response="{\"ok\":true,\"result\":{}}";char *dynamic=NULL;
  if(!strcmp(m,"host.auth.get"))response="{\"ok\":false,\"error\":{\"message\":\"fixture has no backing file\"}}";
  else if(!strcmp(m,"host.auth.list"))response="{\"ok\":true,\"result\":{\"files\":[]}}";
  else if(!strcmp(m,"host.http.operation_open"))response="{\"ok\":true,\"result\":{\"operation_id\":\"op\"}}";
  else if(!strcmp(m,"host.auth.save"))response="{\"ok\":true,\"result\":{}}";
  else if(!strcmp(m,"host.http.do")){
    char *request=malloc(n+1);memcpy(request,q,n);request[n]=0;
    const char *body=strstr(request,"ExchangeToken")?
      "{\"Result\":{\"Token\":\"fixture-refreshed\",\"RefreshToken\":\"fixture-rotated\",\"TokenExpireAt\":4102444800}}":
      "{\"config_info_list\":[{\"config_name\":\"fixture-model\",\"display_config\":{\"display_name\":\"Fixture\"}}]}";
    free(request);char *b64=encoded(body);dynamic=malloc(strlen(b64)+160);
    // Actual CPA host.http.do marshals SDK HTTPResponse with Go field names.
    sprintf(dynamic,"{\"ok\":true,\"result\":{\"StatusCode\":200,\"Headers\":{\"Content-Type\":[\"application/json\"]},\"Body\":\"%s\"}}",b64);
    free(b64);response=dynamic;
  }
  else if(!strcmp(m,"host.http.do_stream")){
    char *request=malloc(n+1);memcpy(request,q,n);request[n]=0;
    codebuddy_stream=strstr(request,"copilot.tencent.com")||strstr(request,"codebuddy.ai");free(request);
    response="{\"ok\":true,\"result\":{\"status_code\":200,\"stream_id\":\"upstream\"}}";
  }
  else if(!strcmp(m,"host.http.stream_read")){
    const char *sse="event:progress_notice\ndata:\"Processing_1791709582\"\n\nevent: output\ndata: {\"response\":\"fixture answer\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}\n\nevent: done\ndata: {}\n\n";
    if(codebuddy_stream)sse="data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fixture answer\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\ndata: [DONE]\n\n";
    char *b64=encoded(sse);dynamic=malloc(strlen(b64)+128);sprintf(dynamic,"{\"ok\":true,\"result\":{\"payload\":\"%s\",\"done\":true}}",b64);free(b64);response=dynamic;
  } else if(!strcmp(m,"host.stream.emit")){fwrite(q,1,n,events);fputc('\n',events);fflush(events);}
  else if(!strcmp(m,"host.stream.close"))InterlockedExchange(&closed,1);
  else if(strcmp(m,"host.http.stream_close")&&strcmp(m,"host.http.cancel")){fprintf(stderr,"unexpected host callback: %s\n",m);return 1;}
  out->len=strlen(response);out->ptr=malloc(out->len);memcpy(out->ptr,response,out->len);free(dynamic);return 0;
}
int main(int argc,char **argv){
  if(argc!=5)return 2;
  HMODULE dll=LoadLibraryA(argv[1]);if(!dll){fprintf(stderr,"LoadLibrary error %lu\n",GetLastError());return 3;}
  init_fn init=(init_fn)GetProcAddress(dll,"cliproxy_plugin_init");host_api host={1,NULL,host_call,free_host};plugin_api plugin={0};if(!init||init(&host,&plugin)||plugin.version!=1)return 4;
  FILE *input=fopen(argv[3],"rb");if(!input)return 5;fseek(input,0,SEEK_END);long n=ftell(input);rewind(input);uint8_t *raw=malloc(n);fread(raw,1,n,input);fclose(input);
  events=fopen(argv[4],"wb");if(!events)return 6;
  buffer response={0};int rc=plugin.call(argv[2],raw,n,&response);fwrite(response.ptr,1,response.len,stdout);plugin.free_buffer(response.ptr,response.len);free(raw);
  if(!strcmp(argv[2],"executor.execute_stream")){for(int i=0;i<500&&!InterlockedCompareExchange(&closed,0,0);i++)Sleep(10);if(!closed)rc=7;}
  fclose(events);
  plugin.shutdown();
  // Process exit owns DLL runtime teardown; never FreeLibrary an active Go runtime.
  return rc;
}
