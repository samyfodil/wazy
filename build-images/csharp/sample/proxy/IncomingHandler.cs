// A wasi:http/proxy component: answers every request with 200 and a fixed body.
// componentize-dotnet generates the bindings for wit/ at build time; this
// class is the export they call.
using System.Text;
using ProxyWorld;
using ProxyWorld.wit.Imports.wasi.http.v0_2_12;

namespace ProxyWorld.wit.Exports.wasi.http.v0_2_12;

public class IncomingHandlerExportsImpl : IIncomingHandlerExports
{
    public static void Handle(ITypesImports.IncomingRequest request, ITypesImports.ResponseOutparam responseOut)
    {
        request.Dispose();

        var response = new ITypesImports.OutgoingResponse(new ITypesImports.Fields());
        var body = response.Body();
        ITypesImports.ResponseOutparam.Set(responseOut,
            Result<ITypesImports.OutgoingResponse, ITypesImports.ErrorCode>.Ok(response));

        using (var stream = body.Write())
        {
            stream.BlockingWriteAndFlush(Encoding.UTF8.GetBytes("hello from csharp\n"));
        }
        ITypesImports.OutgoingBody.Finish(body, null);
    }
}
